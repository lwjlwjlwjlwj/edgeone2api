package auth

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"edgeone2api/internal/upstream"
)

// Session represents a DeepSeek Harness session
type Session struct {
	ConversationID string
	SessionID      string
	CreatedAt      time.Time
	LastUsed       time.Time
	FailedCount    int
	ReqCount       int // total requests served by this session (tool-less turns)
	Client         *upstream.Client

	mu      sync.Mutex // locks the session for exclusive use
	active  bool       // guarded by mu
	bound   bool       // true if this session is bound to a key (guarded by mu)

	// SelectedModel caches the last selectModel payload so repeated requests
	// with the same model skip the redundant RPC (guarded by mu).
	SelectedModel string
}

// Lock locks the session for exclusive use
func (s *Session) Lock() {
	s.mu.Lock()
	s.active = true
	s.LastUsed = time.Now()
}

// Unlock unlocks the session
func (s *Session) Unlock() {
	s.active = false
	s.mu.Unlock()
}

// MarkQuotaExceeded flags the session for immediate recycling on the next
// Release/ReleaseBind call.  Call before releasing when a quota/rate-limit
// error is detected.  Caller must hold the session lock (from Bind/Acquire).
func (s *Session) MarkQuotaExceeded() {
	s.ReqCount = 999999
}

// PoolConfig configures the session pool
type PoolConfig struct {
	MinSize           int
	MaxSize           int
	TTL               time.Duration // session max lifetime
	BindTTL           time.Duration // idle bound-session lifetime
	MaxReqPerSession  int           // max requests before session is recycled (0 = unlimited)
	UpstreamURL       string
	AgentPreset       string // "makers" (default), "minimal", "standard", "code", "cordis"
}

// boundEntry is a session bound to a client key
type boundEntry struct {
	session  *Session
	lastUsed time.Time
}

// Pool manages a pool of sessions shared across clients, plus
// per-key bound sessions for conversation continuity.
type Pool struct {
	mu     sync.RWMutex
	free   []*Session              // sessions available for generic assignment
	binds  map[string]*boundEntry  // key → bound session
	config PoolConfig
}

// NewPool creates a new session pool and eagerly creates min sessions
func NewPool(cfg PoolConfig) *Pool {
	if cfg.BindTTL == 0 {
		cfg.BindTTL = 30 * time.Minute
	}
	if cfg.TTL == 0 {
		cfg.TTL = 60 * time.Minute
	}
	if cfg.MaxReqPerSession == 0 {
		cfg.MaxReqPerSession = 200
	}
	pool := &Pool{
		config: cfg,
		binds:  make(map[string]*boundEntry),
	}
	// Eagerly create minimum sessions
	for i := 0; i < cfg.MinSize; i++ {
		sess, err := pool.createSession()
		if err != nil {
			log.Printf("pool init: create session %d/%d: %v", i+1, cfg.MinSize, err)
			continue
		}
		pool.free = append(pool.free, sess)
	}
	// Start maintenance goroutine
	go pool.maintain()
	return pool
}

func (p *Pool) createSession() (*Session, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Each session gets its own client with a random browser fingerprint,
	// so upstream sees each session as a separate browser.  This bounds the
	// damage of rate limiting to one session instead of the whole pool.
	client := upstream.NewClient(p.config.UpstreamURL)
	convID, sessID, err := client.CreateSession(ctx, p.config.AgentPreset)
	if err != nil {
		return nil, err
	}
	return &Session{
		ConversationID: convID,
		SessionID:      sessID,
		CreatedAt:      time.Now(),
		LastUsed:       time.Now(),
		FailedCount:    0,
		Client:         client,
	}, nil
}

// Acquire gets an available free session, locking it for exclusive use.
// Caller must call Release() when done.
func (p *Pool) Acquire(ctx context.Context) (*Session, error) {
	for {
		p.mu.Lock()
		now := time.Now()
		for _, s := range p.free {
			if now.Sub(s.CreatedAt) > p.config.TTL {
				continue // expired
			}
			if s.mu.TryLock() {
				p.mu.Unlock()
				return s, nil
			}
		}
		// No available session; create one if under max size
		if p.totalLocked() < p.config.MaxSize {
			sess, err := p.createSession()
			if err != nil {
				p.mu.Unlock()
				return nil, fmt.Errorf("create session: %w", err)
			}
			sess.Lock()
			p.free = append(p.free, sess)
			p.mu.Unlock()
			return sess, nil
		}
		p.mu.Unlock()

		// All sessions busy — wait briefly then retry
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(300 * time.Millisecond):
		}
	}
}

// Bind returns the session locked for key, creating or reusing the binding.
// Caller must call ReleaseBind(key, session, ok) when done.
// If the bound session is busy, waits for it (up to ctx deadline).
func (p *Pool) Bind(ctx context.Context, key string) (*Session, error) {
	p.mu.Lock()

	if be, ok := p.binds[key]; ok {
		s := be.session
		if s.mu.TryLock() {
			be.lastUsed = time.Now()
			p.mu.Unlock()
			return s, nil
		}
		// Bound session is busy — wait for it, then retry.
		p.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
		return p.Bind(ctx, key)
	}

	// Need a session for this key: reuse a free one or create new
	now := time.Now()
	for i, s := range p.free {
		if now.Sub(s.CreatedAt) > p.config.TTL {
			continue
		}
		if s.mu.TryLock() {
			p.free = append(p.free[:i], p.free[i+1:]...)
			sess := s
			sess.bound = true
			p.binds[key] = &boundEntry{session: sess, lastUsed: time.Now()}
			p.mu.Unlock()
			return sess, nil
		}
	}
	// No free session available; create one if under max
	if len(p.free)+len(p.binds) >= p.config.MaxSize {
		p.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(300 * time.Millisecond):
		}
		return p.Bind(ctx, key)
	}
	var err error
	sess, err := p.createSession()
	if err != nil {
		p.mu.Unlock()
		return nil, fmt.Errorf("create session: %w", err)
	}
	sess.Lock()
	sess.bound = true
	p.binds[key] = &boundEntry{session: sess, lastUsed: time.Now()}
	p.mu.Unlock()
	return sess, nil
}

// Release releases a free-list session after use.
// The caller holds the session lock (from Acquire); do not lock again here.
func (p *Pool) Release(s *Session, success bool) {
	s.ReqCount++
	if success {
		s.FailedCount = 0
	} else {
		s.FailedCount++
	}
	exceeded := p.config.MaxReqPerSession > 0 && s.ReqCount >= p.config.MaxReqPerSession
	failed := s.FailedCount >= 3

	s.Unlock() // release the lock held since Acquire
	if failed || exceeded {
		if exceeded {
			log.Printf("pool: session %s recycled after %d requests", s.SessionID, s.ReqCount)
		}
		p.removeSession(s)
	}
}

// ReleaseBind releases a bound session after use.
// On success the binding is kept for continuity; on failure it is removed.
// Sessions that hit request/quota limits are recycled and replaced by a fresh
// one on the next Bind — this is the "incognito" rotation that bypasses the
// per-browser-state daily usage cap.
// The caller holds the session lock (from Bind); do not lock again here.
func (p *Pool) ReleaseBind(key string, s *Session, success bool) {
	s.ReqCount++
	if success {
		s.FailedCount = 0
	} else {
		s.FailedCount++
	}
	exceeded := p.config.MaxReqPerSession > 0 && s.ReqCount >= p.config.MaxReqPerSession
	failed := s.FailedCount >= 3

	if !success && failed {
		// Permanent failure — drop the binding so the next request
		// with this key gets a brand-new session automatically.
		p.mu.Lock()
		delete(p.binds, key)
		s.bound = false
		p.mu.Unlock()
		log.Printf("pool: dropped failed binding %s (session %s)", key, s.SessionID)
	}
	if exceeded {
		// Rotate: remove the session from the binding so the next request
		// with the same key creates a fresh session (new conversation id).
		p.mu.Lock()
		delete(p.binds, key)
		s.bound = false
		p.mu.Unlock()
		log.Printf("pool: rotated session %s for key %s after %d requests", s.SessionID, key, s.ReqCount)
		s.Unlock()
		return
	}
	s.Unlock()
}

// releaseToFree returns a bound session to the free list
func (p *Pool) releaseToFree(s *Session) {
	s.mu.Lock()
	s.bound = false
	s.mu.Unlock()
	p.mu.Lock()
	p.free = append(p.free, s)
	p.mu.Unlock()
}

// removeSession drops a failed session entirely
func (p *Pool) removeSession(s *Session) {
	p.mu.Lock()
	if s.bound {
		for k, be := range p.binds {
			if be.session == s {
				delete(p.binds, k)
			}
		}
	} else {
		for i, x := range p.free {
			if x == s {
				p.free = append(p.free[:i], p.free[i+1:]...)
				break
			}
		}
	}
	p.mu.Unlock()
}

// totalLocked returns the total number of sessions managed (free + bound).
// Caller must hold p.mu.
func (p *Pool) totalLocked() int {
	return len(p.free) + len(p.binds)
}

// Count returns the total number of sessions managed
func (p *Pool) Count() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.totalLocked()
}

// Stats returns pool statistics
func (p *Pool) Stats() map[string]interface{} {
	p.mu.RLock()
	defer p.mu.RUnlock()

	stats := map[string]interface{}{
		"free":    len(p.free),
		"bound":   len(p.binds),
		"total":   p.totalLocked(),
		"min":     p.config.MinSize,
		"max":     p.config.MaxSize,
		"ttl_s":   p.config.TTL.Seconds(),
		"bind_ttl_s": p.config.BindTTL.Seconds(),
		"active":  0,
		"failed":  0,
	}
	active, failed := 0, 0
	for _, s := range p.free {
		s.mu.Lock()
		if s.active {
			active++
		}
		if s.FailedCount > 0 {
			failed++
		}
		s.mu.Unlock()
	}
	for _, be := range p.binds {
		s := be.session
		s.mu.Lock()
		if s.active {
			active++
		}
		if s.FailedCount > 0 {
			failed++
		}
		s.mu.Unlock()
	}
	stats["active"] = active
	stats["failed"] = failed
	return stats
}

// maintain periodically cleans up expired sessions and idle bindings
func (p *Pool) maintain() {
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()

	for range ticker.C {
		now := time.Now()
		p.mu.Lock()

		// Drop expired free sessions
		kept := make([]*Session, 0, len(p.free))
		for _, s := range p.free {
			if now.Sub(s.CreatedAt) > p.config.TTL {
				continue
			}
			kept = append(kept, s)
		}
		p.free = kept

		// Reap idle bound sessions
		for k, be := range p.binds {
			if now.Sub(be.lastUsed) > p.config.BindTTL || now.Sub(be.session.CreatedAt) > p.config.TTL {
				if be.session.mu.TryLock() {
					delete(p.binds, k)
					be.session.bound = false
					be.session.mu.Unlock()
					p.free = append(p.free, be.session)
					log.Printf("pool: reaped idle binding %s", k)
				}
			}
		}

		// Ensure minimum free sessions
		for len(p.free) < p.config.MinSize && p.totalLocked() < p.config.MaxSize {
			sess, err := p.createSession()
			if err != nil {
				log.Printf("maintain: create session: %v", err)
				break
			}
			p.free = append(p.free, sess)
		}

		count := p.totalLocked()
		freeCount := len(p.free)
		bound := len(p.binds)
		p.mu.Unlock()
		log.Printf("pool maintenance: %d sessions (free=%d, bound=%d)", count, freeCount, bound)
	}
}

// Close closes the pool
func (p *Pool) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.free = nil
	p.binds = make(map[string]*boundEntry)
}