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
	"cloudlynet_edgeagent/goagent/internal/cwmp"
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

	manifest, err := cwmp.LoadManifest()
	if err != nil {
		log.Fatalf("cwmp manifest load failed: %v", err)
	}

	// The agent IS the ACS: buffer.Buffer backs the CWMP param/event store, and
	// the server binds the exact :7547 the NanoLink already dials.
	acs := cwmp.NewServer(cfg.CWMP.Listen, buf)
	acs.UseConnRequest(cfg.CWMP.CRUser, cfg.CWMP.CRPass, cfg.CWMP.CRURLOverride)
	if cfg.CWMP.FullSnapshotOnFirstContact {
		// Read the managed-config catalogue on a device's first contact so the
		// first config snapshot is available without waiting for the periodic cycle.
		acs.UseFirstContactSnapshot(collector.SnapshotPaths)
	}

	cl := collector.New(acs, ruleEngine, cfg.FTPWatchDir)
	w := worker.New(cfg, cloud.New(cfg.Enrollment.BaseURL, cfg.Enrollment.APIKey), acs, manifest, buf, cl)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// A dead ACS listener means no config can flow — treat a bind failure as fatal.
	go func() {
		if err := acs.Run(); err != nil {
			log.Fatalf("cwmp server: %v", err)
		}
	}()

	if err := w.Run(ctx); err != nil && ctx.Err() == nil {
		log.Fatalf("agent stopped: %v", err)
	}
}
