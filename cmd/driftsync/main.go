package main

import (
	"context"
	"flag"
	"log"
	"os"
	"path/filepath"

	"github.com/xiaochun-z/driftsync/internal/auth"
	"github.com/xiaochun-z/driftsync/internal/config"
	"github.com/xiaochun-z/driftsync/internal/graph"
	"github.com/xiaochun-z/driftsync/internal/store"
	"github.com/xiaochun-z/driftsync/internal/syncer"
)

var version = "v0.6.2"

func main() {
	cfgPathFlag := flag.String("config", "", "Path to configuration YAML (optional)")
	flag.Parse()

	cfgPath := *cfgPathFlag
	if cfgPath == "" {
		cfgPath = "config.yaml"
	}

	// Verify config file existence
	if _, err := os.Stat(cfgPath); os.IsNotExist(err) {
		log.Fatalf("Config file not found: %s", cfgPath)
	}

	// Load configuration
	cfg, err := config.Load(cfgPath)
	if err != nil {
		log.Fatalf("Failed to load config %s: %v", cfgPath, err)
	}
	if err := cfg.Validate(); err != nil {
		log.Fatalf("Config invalid: %v", err)
	}

	log.Printf("DriftSync %s (one-shot) starting. config=%s local_path=%s tenant=%s",
		version, cfgPath, cfg.LocalPath, cfg.Tenant)

	// Database in same directory as config.yaml
	dbPath := filepath.Join(filepath.Dir(cfgPath), "driftsync.db")
	db, err := store.Open(dbPath)
	if err != nil {
		log.Fatalf("open db: %v", err)
	}
	defer db.Close()

	tokStore := store.NewTokenStore(db)
	authClient := auth.NewDeviceCodeClient(cfg.Tenant, cfg.ClientID, tokStore)

	ctx := context.Background()
	httpClient := authClient.AuthorizedClient(ctx)
	g := graph.NewClient(httpClient)

	if err := authClient.EnsureLogin(ctx); err != nil {
		log.Fatalf("login failed: %v", err)
	}

	s := syncer.NewSyncer(cfg, db, g)

	if err := s.SyncOnce(ctx); err != nil {
		log.Fatalf("sync error: %v", err)
	}

	log.Printf("Sync completed. Database stored at: %s", dbPath)
}
