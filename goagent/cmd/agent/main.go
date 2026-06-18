package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"

	"cloudlynet_edgeagent/goagent/internal/buffer"
	"cloudlynet_edgeagent/goagent/internal/cloud"
	"cloudlynet_edgeagent/goagent/internal/collector"
	"cloudlynet_edgeagent/goagent/internal/config"
	"cloudlynet_edgeagent/goagent/internal/genieacs"
	"cloudlynet_edgeagent/goagent/internal/rules"
	"cloudlynet_edgeagent/goagent/internal/worker"
)

func main() {
	configPath := flag.String("config", "/etc/cloudlynet-agent/agent.yaml", "path to agent yaml")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("config load failed: %v", err)
	}

	buf, err := buffer.Open(cfg.BufferDB, cfg.BufferMaxBytes)
	if err != nil {
		log.Fatalf("buffer open failed: %v", err)
	}
	defer buf.Close()

	ruleEngine, err := rules.Load(cfg.RulesFile)
	if err != nil {
		log.Printf("rules load failed; using defaults: %v", err)
		ruleEngine = rules.DefaultEngine()
	}

	nbi := genieacs.New(cfg.GenieACSNBIURL)
	cl := collector.New(nbi, ruleEngine, cfg.FTPWatchDir)
	w := worker.New(cfg, cloud.New(cfg.Enrollment.BaseURL, cfg.Enrollment.APIKey), nbi, buf, cl)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := w.Run(ctx); err != nil && ctx.Err() == nil {
		log.Fatalf("agent stopped: %v", err)
	}
}
