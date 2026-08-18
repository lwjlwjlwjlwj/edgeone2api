package main

import (
	"flag"
	"log"
	"net/http"
	"time"

	"edgeone2api/internal/auth"
	"edgeone2api/internal/config"
	"edgeone2api/internal/server"
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

	srv := server.New(pool, cfg.APIKey, cfg.Models, 180*time.Second, cfg.ModelMap)
	httpServer := &http.Server{
		Addr:              cfg.Listen,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	log.Printf("listening on %s", cfg.Listen)
	if err := httpServer.ListenAndServe(); err != nil {
		log.Fatalf("server: %v", err)
	}
}