package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/xiaochun-z/driftsync/internal/auth"
	"github.com/xiaochun-z/driftsync/internal/config"
	"github.com/xiaochun-z/driftsync/internal/graph"
	"github.com/xiaochun-z/driftsync/internal/store"
	"github.com/xiaochun-z/driftsync/internal/syncer"
)

// version is the application version.
// It can optionally be overridden at build time via:
//
//	go build -ldflags="-X main.version=..."
//
// If not overridden, this static value is used.
var version = "v0.7.14"

// printUsage prints a help message that includes the version, usage, and flags.
// It is wired into flag.Usage and used for -h/--help as well as parse errors.
func printUsage() {
	exe := filepath.Base(os.Args[0])
	out := flag.CommandLine.Output()

	// Header: name + version
	fmt.Fprintf(out, "%s %s\n\n", exe, version)

	// Basic usage line
	fmt.Fprintf(out, "Usage:\n  %s [flags]\n\n", exe)

	// Optional short description
	fmt.Fprintln(out, "driftsync synchronizes a local folder with a cloud drive using delta API and a local SQLite database.")

	// List all flags
	fmt.Fprintln(out, "Flags:")
	flag.PrintDefaults()
}

func main() {
	// Install custom Usage printer before defining/using flags.
	flag.Usage = printUsage

	// Flags
	cfgPathFlag := flag.String("config", "", "Path to configuration YAML (optional)")
	interactiveFlag := flag.Bool("interactive", false, "Enable interactive conflict resolution")
	flag.BoolVar(interactiveFlag, "i", false, "Alias for -interactive")
	versionFlag := flag.Bool("version", false, "Print version and exit")

	flag.Parse()

	// Handle --version / -version early, before touching config or networking.
	if *versionFlag {
		out := flag.CommandLine.Output()
		exe := filepath.Base(os.Args[0])
		fmt.Fprintf(out, "%s %s\n", exe, version)
		return
	}

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
	// CLI flag overrides config
	if *interactiveFlag {
		cfg.Interactive = true
	}

	if err := cfg.Validate(); err != nil {
		log.Fatalf("Config invalid: %v", err)
	}

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
