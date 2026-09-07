package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"edgeone2api/internal/auth"
	"edgeone2api/internal/config"
	"edgeone2api/internal/server"
	"edgeone2api/internal/toolcall"
)

func main() {
	configPath := flag.String("config", "", "config file path (JSON)")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

	log.Printf("edgeone2api starting: listen=%s models=%v pool=[%d,%d] ttl=%dm bind_ttl=%dm max_req=%d upstream=%s agent_preset=%s",
		cfg.Listen, cfg.Models, cfg.PoolMin, cfg.PoolMax, cfg.TTLMin, cfg.BindTTLMin, cfg.MaxReqPerSession, cfg.UpstreamURL, cfg.AgentPreset)

	poolCfg := auth.PoolConfig{
		MinSize:          cfg.PoolMin,
		MaxSize:          cfg.PoolMax,
		TTL:              time.Duration(cfg.TTLMin) * time.Minute,
		FreeTTL:          time.Duration(cfg.FreeTTLMin) * time.Minute,
		BindTTL:          time.Duration(cfg.BindTTLMin) * time.Minute,
		MaxReqPerSession: cfg.MaxReqPerSession,
		UpstreamURL:      cfg.UpstreamURL,
		AgentPreset:      cfg.AgentPreset,
	}
	pool := auth.NewPool(poolCfg)
	defer pool.Close()

	// Wait for initial sessions
	time.Sleep(2 * time.Second)
	log.Printf("session pool ready: %d session(s)", pool.Count())

	// Tool-call sidecar (the ported toolforge XYML engine).  A hard
	// dependency for tool-mode requests; fail fast so misconfiguration is
	// visible at boot instead of at request time.
	tc := toolcall.NewClient()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := tc.Start(ctx, 15*time.Second); err != nil {
		log.Fatalf("tool sidecar: %v", err)
	}
	defer tc.Stop()

	srv := server.New(pool, cfg.APIKey, cfg.Models, 180*time.Second, cfg.ModelMap, tc)
	httpServer := &http.Server{
		Addr:              cfg.Listen,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	// Shut the sidecar down cleanly on SIGINT/SIGTERM or bind failure so a
	// restart never hits "address already in use" from an orphaned child.
	sigCtx, sigStop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer sigStop()
	go func() {
		<-sigCtx.Done()
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutdownCancel()
		_ = httpServer.Shutdown(shutdownCtx)
	}()

	log.Printf("listening on %s", cfg.Listen)
	if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Printf("server: %v", err)
		tc.Stop()
		pool.Close()
		os.Exit(1)
	}
}
