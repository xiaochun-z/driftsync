package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/xiaochun-z/driftsync/internal/auth"
	"github.com/xiaochun-z/driftsync/internal/config"
	"github.com/xiaochun-z/driftsync/internal/graph"
	"github.com/xiaochun-z/driftsync/internal/store"
	"github.com/xiaochun-z/driftsync/internal/syncer"
)

// version is the application version.
// Override at build time via: go build -ldflags="-X main.version=v1.2.3"
var version = "dev"

func printUsage() {
	exe := filepath.Base(os.Args[0])
	out := flag.CommandLine.Output()
	fmt.Fprintf(out, "%s %s\n\n", exe, version)
	fmt.Fprintf(out, "Usage:\n  %s [flags]\n\n", exe)
	fmt.Fprintln(out, "driftsync synchronizes a local folder with a cloud drive using delta API and a local SQLite database.")
	fmt.Fprintln(out, "Flags:")
	flag.PrintDefaults()
}

func main() {
	flag.Usage = printUsage

	cfgPathFlag := flag.String("config", "", "Path to configuration YAML (optional)")
	interactiveFlag := flag.Bool("interactive", false, "Enable interactive conflict resolution")
	flag.BoolVar(interactiveFlag, "i", false, "Alias for -interactive")
	versionFlag := flag.Bool("version", false, "Print version and exit")

	flag.Parse()

	if *versionFlag {
		fmt.Fprintf(flag.CommandLine.Output(), "%s %s\n", filepath.Base(os.Args[0]), version)
		return
	}

	cfgPath := *cfgPathFlag
	if cfgPath == "" {
		cfgPath = "config.yaml"
	}

	// Provide an actionable hint if the config file is missing.
	if _, err := os.Stat(cfgPath); os.IsNotExist(err) {
		hint := ""
		sample := filepath.Join(filepath.Dir(cfgPath), "config.sample.yaml")
		if _, serr := os.Stat(sample); serr == nil {
			hint = fmt.Sprintf("\n  Hint: cp %s %s  (then edit it)", sample, cfgPath)
		}
		log.Fatalf("Config file not found: %s%s", cfgPath, hint)
	}

	cfg, err := config.Load(cfgPath)
	if err != nil {
		log.Fatalf("Failed to load config %s: %v", cfgPath, err)
	}
	if *interactiveFlag {
		cfg.Interactive = true
	}
	if err := cfg.Validate(); err != nil {
		log.Fatalf("Config invalid: %v", err)
	}

	// Cancellable context: Ctrl+C or SIGTERM trigger graceful shutdown.
	// In-flight downloads/uploads respect ctx and will stop cleanly.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	dbPath := filepath.Join(filepath.Dir(cfgPath), "driftsync.db")
	db, err := store.Open(dbPath)
	if err != nil {
		log.Fatalf("open db: %v", err)
	}
	defer db.Close()

	tokStore := store.NewTokenStore(db)
	authClient := auth.NewDeviceCodeClient(cfg.Tenant, cfg.ClientID, tokStore)

	httpClient := authClient.AuthorizedClient(ctx)
	g := graph.NewClient(httpClient)

	if err := authClient.EnsureLogin(ctx); err != nil {
		log.Fatalf("login failed: %v", err)
	}

	s := syncer.NewSyncer(cfg, db, g)

	if err := s.SyncOnce(ctx); err != nil {
		if ctx.Err() != nil {
			log.Printf("Sync interrupted by user.")
		} else {
			log.Printf("sync error: %v", err)
		}
		os.Exit(1)
	}

	log.Printf("Sync completed. Database stored at: %s", dbPath)
}
