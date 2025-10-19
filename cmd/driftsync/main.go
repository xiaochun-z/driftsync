package main

import (
	"context"
	"log"

	"github.com/xiaochun-z/driftsync/internal/auth"
	"github.com/xiaochun-z/driftsync/internal/config"
	"github.com/xiaochun-z/driftsync/internal/graph"
	"github.com/xiaochun-z/driftsync/internal/store"
	"github.com/xiaochun-z/driftsync/internal/syncer"
)

var version = "v0.6"

func main() {
	cfg, err := config.Load("config.yaml")
	if err != nil {
		log.Printf("config.yaml not found or invalid: %v", err)
		cfg = config.FromEnvFallback()
	}
	if err := cfg.Validate(); err != nil {
		log.Fatalf("config invalid: %v", err)
	}
	log.Printf("DriftSync %s (one-shot) starting. local_path=%s tenant=%s", version, cfg.LocalPath, cfg.Tenant)

	db, err := store.Open("driftsync.db")
	if err != nil { log.Fatalf("open db: %v", err) }
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
	log.Println("Sync completed. Exiting.")
}
