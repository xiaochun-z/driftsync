// Package syncer orchestrates bidirectional synchronization between a local
// directory and OneDrive. The sync cycle is:
//
//  1. Propagate local deletions to the cloud.
//  2. Apply cloud changes (downloads, cloud-side deletes) via the Delta API.
//  3. Upload new or modified local files.
package syncer

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/xiaochun-z/driftsync/internal/config"
	"github.com/xiaochun-z/driftsync/internal/graph"
	"github.com/xiaochun-z/driftsync/internal/scan"
	"github.com/xiaochun-z/driftsync/internal/selective"
	"github.com/xiaochun-z/driftsync/internal/store"
	"github.com/xiaochun-z/driftsync/internal/ui"
)

const (
	// smallFileThreshold is the max size for a single-request PUT upload.
	// Files larger than this use the resumable session API.
	smallFileThreshold int64 = 4 * 1024 * 1024

	// recently* TTLs suppress echo downloads/uploads for files just synced.
	recentlyDownloadTTL    = 30 * time.Second
	recentlyUploadTTL      = 60 * time.Second
	loaderInterval         = 120 * time.Millisecond
	defaultDownloadWorkers = 8
	defaultUploadWorkers   = 4
)

// dlTask describes a single file to be downloaded from the cloud.
type dlTask struct {
	ID      string
	PathRel string
	Size    int64
	ETag    string
	Sha256  string
	ModTime time.Time
}

// Syncer holds all state for a single sync session.
type Syncer struct {
	cfg    *config.Config
	db     *sql.DB
	g      *graph.Client
	filter *selective.List

	deltaLink string
	lastLocal map[string]scan.Entry // mtime+size cache from last upload scan

	// recently tracks files synced within their TTL to suppress echo re-syncs.
	recently map[string]int64 // path → Unix expiry timestamp
	mu       sync.Mutex       // guards recently, uploaded, downloaded, deleted

	dbMu sync.Mutex // serialises all DB writes

	// OnSyncEvent is an optional callback fired when a file is successfully synced.
	OnSyncEvent func(action, rel string)

	uploaded   []string
	downloaded []string
	deleted    []string

	loader *ui.Loader
}

// NewSyncer constructs a Syncer. The filter is built from the config's sync
// rules: the YAML sync: section takes precedence over sync_list_path.
func NewSyncer(cfg *config.Config, db *sql.DB, g *graph.Client) *Syncer {
	var f *selective.List
	if cfg.Sync != nil {
		f = selective.FromYAML(cfg.Sync.Include, cfg.Sync.Exclude)
	} else if strings.TrimSpace(cfg.SyncListPath) != "" {
		lf, err := selective.Load(cfg.SyncListPath)
		if err != nil {
			log.Printf("WARN: load sync_list_path failed: %v", err)
		} else {
			f = lf
		}
	}
	return &Syncer{
		cfg:       cfg,
		db:        db,
		g:         g,
		filter:    f,
		lastLocal: map[string]scan.Entry{},
		recently:  map[string]int64{},
	}
}

// SyncOnce runs a full sync cycle: local-delete propagation → cloud delta → local upload.
func (s *Syncer) SyncOnce(ctx context.Context) error {
	s.loader = ui.Start(loaderInterval)
	defer s.loader.Stop("")

	if err := os.MkdirAll(s.cfg.LocalPath, 0o755); err != nil {
		return fmt.Errorf("create local root: %w", err)
	}

	// Phase 0: cleanup local files that were previously synced but are now excluded.
	if err := s.localCleanupExcluded(ctx); err != nil {
		log.Printf("local cleanup excluded: %v", err)
	}

	// Phase 1: push local deletions to cloud first, so the cloud cannot
	// resurrect a file the user intentionally deleted.
	if err := s.localDetectAndDeleteCloud(ctx); err != nil {
		log.Printf("local→cloud delete: %v", err)
	}

	// Phase 2: apply cloud changes.
	if s.cfg.DownloadFromCloud {
		if s.deltaLink == "" {
			if val, err := store.GetMeta(ctx, s.db, "delta_link"); err == nil {
				s.deltaLink = val
			}
		}
		if err := s.cloudDelta(ctx); err != nil {
			return err
		}
	}

	// Phase 3: upload new/modified local files.
	if s.cfg.UploadFromLocal {
		if err := s.localScanAndUpload(ctx); err != nil {
			log.Printf("local upload: %v", err)
		}
	}

	s.loader.Stop("")
	s.printSummary()
	return nil
}
