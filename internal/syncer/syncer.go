package syncer

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"os"
	"path/filepath"
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

// dlTask is defined at package level to ensure type consistency
type dlTask struct {
	ID      string
	PathRel string
	Size    int64
	ETag    string
	CTag    string
	Sha256  string
	ModTime time.Time
}

type Syncer struct {
	cfg        *config.Config
	db         *sql.DB
	g          *graph.Client
	deltaLink  string
	lastLocal  map[string]scan.Entry
	filter     *selective.List
	recently   map[string]int64
	mu         sync.Mutex
	dbMu       sync.Mutex // Serializes DB writes
	uploaded   []string
	downloaded []string
	deleted    []string
	loader     *ui.Loader
}

func NewSyncer(cfg *config.Config, db *sql.DB, g *graph.Client) *Syncer {
	var f *selective.List
	if cfg.Sync != nil {
		f = selective.FromYAML(cfg.Sync.Include, cfg.Sync.Exclude)
	} else {
		if strings.TrimSpace(cfg.SyncListPath) != "" {
			if lf, err := selective.Load(cfg.SyncListPath); err == nil {
				f = lf
			} else {
				log.Printf("WARN: load sync_list_path failed: %v", err)
			}
		}
	}
	return &Syncer{
		cfg:        cfg,
		db:         db,
		g:          g,
		filter:     f,
		lastLocal:  map[string]scan.Entry{},
		recently:   map[string]int64{},
		uploaded:   []string{},
		downloaded: []string{},
		deleted:    []string{},
	}
}

// SyncOnce executes a full synchronization cycle safely.
func (s *Syncer) SyncOnce(ctx context.Context) error {
	s.loader = ui.Start(120 * time.Millisecond)
	defer s.loader.Stop("")

	if err := os.MkdirAll(s.cfg.LocalPath, 0o755); err != nil {
		return fmt.Errorf("create local root error: %w", err)
	}

	// 1. Cloud First: Check for updates/deletes from cloud.
	if s.cfg.DownloadFromCloud {
		if s.deltaLink == "" {
			val, err := store.GetMeta(ctx, s.db, "delta_link")
			if err == nil && val != "" {
				s.deltaLink = val
			}
		}
		if err := s.cloudDelta(ctx); err != nil {
			return err
		}
	}

	// 2. Local Deletes: Propagate local deletions to cloud.
	if err := s.localDetectAndDeleteCloud(ctx); err != nil {
		log.Printf("local->cloud delete err: %v", err)
	}

	// 3. Local Uploads: Upload new/modified local files.
	if s.cfg.UploadFromLocal {
		if err := s.localScanAndUpload(ctx); err != nil {
			log.Printf("local upload err: %v", err)
		}
	}

	s.loader.Stop("")
	s.printSummary()
	return nil
}

// cloudDelta handles incoming changes from the cloud.
func (s *Syncer) cloudDelta(ctx context.Context) error {
	isIncremental := s.deltaLink != ""

	var toDownload []dlTask
	cloudAlive := map[string]struct{}{}

	// Paging loop for Delta API
	for {
		d, err := s.g.RootDelta(ctx, s.deltaLink)
		if err != nil {
			return err
		}
		for _, it := range d.Value {
			// === HANDLE CLOUD DELETION ===
			// Process deletion BEFORE path check, because deleted items often lack Name/Parent,
			// causing itemPathRel to be empty. We must recover the path from DB via ID.
			if it.Deleted != nil {
				dbOld, _ := store.GetByID(ctx, s.db, it.ID)
				
				var pathRel string
				if dbOld != nil {
					// Trust DB path
					pathRel = dbOld.PathRel
				} else {
					// Fallback to cloud path (likely empty, but try anyway)
					pathRel = s.itemPathRel(it)
				}

				// If we still don't have a path, we can't delete anything.
				if pathRel == "" || pathRel == "." || pathRel == "/" {
					continue
				}

				localPath := filepath.Join(s.cfg.LocalPath, filepath.FromSlash(pathRel))

				// SAFETY CHECK: Verify if local file matches DB before deleting.
				shouldDelete := true
				if _, err := os.Stat(localPath); err == nil {
					// File exists locally. Check if it's dirty.
					localHash, _ := scan.HashFile(localPath)

					isDirty := false
					if dbOld == nil {
						// Cloud says delete, but we have a local file not in DB?
						// It's a new local file. Don't delete.
						isDirty = true
					} else if localHash != dbOld.Shasum {
						// Content changed locally since last sync.
						isDirty = true
					}

					if isDirty {
						log.Printf("[SAFETY] Skipping cloud deletion for %s: Local file has modifications.", pathRel)
						// Remove DB entry so it's treated as a new local file next time
						shouldDelete = false
						s.dbMu.Lock()
						_ = store.DeleteByPath(ctx, s.db, pathRel)
						s.dbMu.Unlock()
					}
				}

				if shouldDelete {
					// Perform Safe Delete (Move to Trash)
					if err := s.moveToTrash(localPath); err != nil {
						log.Printf("trash error %s: %v", pathRel, err)
					} else {
						s.trackChange("delete (cloud)", pathRel, 0)
					}

					s.dbMu.Lock()
					_ = store.DeleteByPath(ctx, s.db, pathRel)
					s.dbMu.Unlock()
				}
				continue
			}

			// === NORMAL ITEM PATH CHECK ===
			pathRel := s.itemPathRel(it)
			// FIX: Ignore root or empty paths to prevent weird logs/behavior
			if pathRel == "" || pathRel == "." || pathRel == "/" {
				continue
			}
			localPath := filepath.Join(s.cfg.LocalPath, filepath.FromSlash(pathRel))

			// === HANDLE FOLDERS ===
			if it.Folder != nil {
				if s.filter != nil && !s.filter.ShouldSync(pathRel, true) {
					continue
				}
				_ = os.MkdirAll(localPath, 0o755)
				cloudAlive[pathRel] = struct{}{}
				continue
			}

			// === HANDLE FILES (Potential Download) ===
			if it.File != nil {
				if s.filter != nil && !s.filter.ShouldSync(pathRel, false) {
					continue
				}
				if isInternalConflictFile(pathRel) {
					continue
				}

				mt, _ := time.Parse(time.RFC3339, it.LastModifiedDateTime)
				sha := ""
				if it.File != nil && it.File.Hashes != nil {
					sha = strings.ToLower(it.File.Hashes.Sha256Hash)
				}
				toDownload = append(toDownload, dlTask{
					ID: it.ID, PathRel: pathRel, Size: it.Size,
					ETag: it.ETag, CTag: it.CTag, ModTime: mt, Sha256: sha,
				})
				cloudAlive[pathRel] = struct{}{}
			}
		}

		if d.NextLink != "" {
			s.deltaLink = d.NextLink
			s.saveDeltaLink(ctx, s.deltaLink)
			continue
		}
		s.deltaLink = d.DeltaLink
		s.saveDeltaLink(ctx, s.deltaLink)
		break
	}

	// === RECONCILE (Full Sync Only) ===
	if !isIncremental {
		if err := s.reconcileMissing(ctx, cloudAlive); err != nil {
			log.Printf("reconcile error: %v", err)
		}
	}

	if len(toDownload) == 0 {
		return nil
	}

	// === PROCESS DOWNLOADS ===
	// Pass concrete slice, no interface{} conversion
	return s.processDownloads(ctx, toDownload)
}

func (s *Syncer) reconcileMissing(ctx context.Context, cloudAlive map[string]struct{}) error {
	dbPaths, err := store.ListAllPaths(ctx, s.db)
	if err != nil {
		return err
	}
	for _, p := range dbPaths {
		if p == "" || p == "." { continue }
		if s.filter != nil && !s.filter.ShouldSync(p, false) {
			continue
		}
		if _, ok := cloudAlive[p]; !ok {
			// File is in DB but not in Cloud Scan -> Cloud Deleted it.
			lp := filepath.Join(s.cfg.LocalPath, filepath.FromSlash(p))
			
			if _, statErr := os.Stat(lp); statErr == nil {
				dbOld, _ := store.GetByPathFull(ctx, s.db, p)
				localHash, _ := scan.HashFile(lp)
				
				// If local modified, keep it (Untrack from DB)
				if dbOld != nil && localHash != dbOld.Shasum {
					log.Printf("[SAFETY] Reconcile: Keeping %s (modified locally)", p)
					s.dbMu.Lock()
					_ = store.DeleteByPath(ctx, s.db, p)
					s.dbMu.Unlock()
					continue
				}
				
				// Safe to delete
				_ = s.moveToTrash(lp)
				s.trackChange("delete (reconcile)", p, 0)
			}
			s.dbMu.Lock()
			_ = store.DeleteByPath(ctx, s.db, p)
			s.dbMu.Unlock()
			s.clearRecently(p)
		}
	}
	return nil
}

// processDownloads now accepts []dlTask directly, preventing interface conversion panic
func (s *Syncer) processDownloads(ctx context.Context, dlTasks []dlTask) error {
	workers := s.cfg.DownloadWorkers
	if workers < 1 { workers = 8 }
	
	jobs := make(chan dlTask, len(dlTasks))
	errCh := make(chan error, workers)
	var wg sync.WaitGroup

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for t := range jobs {
				if err := s.downloadWorker(ctx, t.ID, t.PathRel, t.ETag, t.Sha256, t.Size); err != nil {
					log.Printf("download worker error: %v", err)
				}
			}
			errCh <- nil
		}()
	}

	for _, t := range dlTasks {
		jobs <- t
	}
	close(jobs)
	wg.Wait()
	close(errCh)
	return nil
}

func (s *Syncer) downloadWorker(ctx context.Context, id, rel, etag, remoteSha256 string, size int64) error {
	lp := filepath.Join(s.cfg.LocalPath, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(lp), 0o755); err != nil {
		return err
	}

	dbOld, _ := store.GetByPathFull(ctx, s.db, rel)
	
	// Check Local State
	localHash := ""
	localExists := false
	if info, err := os.Stat(lp); err == nil {
		localExists = true
		// Optimization: If meta matches DB, assume hash matches DB
		if dbOld != nil && info.ModTime().Unix() == dbOld.Mtime && info.Size() == dbOld.Size {
			localHash = dbOld.Shasum
		} else {
			localHash, _ = scan.HashFile(lp)
		}
	}

	// Conflict Detection Logic
	conflict := false
	
	if localExists && dbOld != nil {
		localChanged := localHash != dbOld.Shasum
		cloudChanged := etag != dbOld.ETag
		
		if localChanged && cloudChanged {
			conflict = true
		}
	} else if localExists && dbOld == nil {
		// Local file exists but no DB record. Cloud wants to download.
		// Assume conflict.
		conflict = true 
	}

	targetPath := lp
	if conflict {
		choice := ui.KeepBoth
		if s.cfg.Interactive {
			choice = ui.ResolveConflict(s.loader, rel, "Download")
		}

		switch choice {
		case ui.UseLocal:
			log.Printf("CONFLICT [Keep Local]: Skipping download for %s", rel)
			return nil
		case ui.UseCloud:
			log.Printf("CONFLICT [Overwrite]: Downloading cloud version to %s", rel)
			// targetPath = lp
		case ui.KeepBoth:
			targetPath = s.makeConflictPath(lp, "cloud")
			log.Printf("CONFLICT [Keep Both]: Downloading to %s", targetPath)
		}
	}

	// Skip if identical (Fast ETag check)
	if dbOld != nil && dbOld.ETag == etag && localHash == dbOld.Shasum {
		return nil
	}

	// Smart Sync: If ETag changed but Content matches, update DB and skip download
	if localExists && remoteSha256 != "" && localHash == remoteSha256 {
		log.Printf("[SMART] Metadata update only (content match): %s", rel)
		s.dbMu.Lock()
		_ = store.UpsertItem(ctx, s.db, store.Item{
			ID: id, PathRel: rel, ETag: etag, Size: size, Mtime: 0, // Mtime 0 keeps original if possible, or use current
			Shasum: localHash, LastSrc: "cloud", LastSync: time.Now().Unix(),
		})
		s.dbMu.Unlock()
		return nil
	}

	// Perform Download
	if err := s.g.DownloadTo(ctx, id, targetPath); err != nil {
		return err
	}

	// Post-Download: Update DB if we updated the canonical file
	newHash, _ := scan.HashFile(targetPath)
	if targetPath == lp {
		s.dbMu.Lock()
		_ = store.UpsertItem(ctx, s.db, store.Item{
			ID: id, PathRel: rel, ETag: etag, Size: size, Mtime: 0,
			Shasum: newHash, LastSrc: "cloud", LastSync: time.Now().Unix(),
		})
		s.dbMu.Unlock()
	}

	s.setRecently(rel, 30*time.Second)
	s.trackChange("download", rel, size)
	return nil
}

func (s *Syncer) moveToTrash(path string) error {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return nil
	}

	// Double check we are not deleting the trash folder itself (paranoia check)
	if strings.Contains(path, ".driftsync_trash") {
		return nil
	}

	trashDir := filepath.Join(s.cfg.LocalPath, ".driftsync_trash")
	if err := os.MkdirAll(trashDir, 0o755); err != nil {
		return err
	}

	base := filepath.Base(path)
	ts := time.Now().Format("20060102-150405")
	dest := filepath.Join(trashDir, fmt.Sprintf("%s.%s.deleted", base, ts))

	return os.Rename(path, dest)
}

func (s *Syncer) localDetectAndDeleteCloud(ctx context.Context) error {
	cur := map[string]struct{}{}
	en, err := scan.ScanDir(s.cfg.LocalPath)
	if err != nil {
		return err
	}
	for _, e := range en {
		if !e.IsDir {
			cur[e.PathRel] = struct{}{}
		}
	}

	rows, err := s.db.QueryContext(ctx, `SELECT path_rel FROM items`)
	if err != nil { return err }
	defer rows.Close()

	var toDelete []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil { return err }
		if p == "" || p == "." { continue }
		if s.filter != nil && !s.filter.ShouldSync(p, false) { continue }

		if _, exists := cur[p]; !exists {
			toDelete = append(toDelete, p)
		}
	}

	for _, rel := range toDelete {
		lp := filepath.Join(s.cfg.LocalPath, filepath.FromSlash(rel))

		// SAFETY DOUBLE-CHECK
		if _, err := os.Stat(lp); err == nil {
			log.Printf("[SAFETY] Aborting cloud delete for %s: File exists locally.", rel)
			continue
		} else if !os.IsNotExist(err) {
			log.Printf("[SAFETY] Aborting cloud delete for %s: FS Error %v", rel, err)
			continue
		}

		if err := s.g.DeleteByPath(ctx, "/"+rel); err != nil {
			log.Printf("cloud delete FAIL %s: %v", rel, err)
			continue
		}

		s.trackChange("delete (local->cloud)", rel, 0)
		s.dbMu.Lock()
		_ = store.DeleteByPath(ctx, s.db, rel)
		s.dbMu.Unlock()
	}
	return nil
}

func (s *Syncer) localScanAndUpload(ctx context.Context) error {
	en, err := scan.ScanDir(s.cfg.LocalPath)
	if err != nil { return err }

	workers := s.cfg.UploadWorkers
	if workers < 1 { workers = 4 }

	type upTask struct { E scan.Entry }
	jobs := make(chan upTask, workers*2)
	errCh := make(chan error, workers)
	var wg sync.WaitGroup

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for t := range jobs {
				if err := s.uploadWorker(ctx, t.E); err != nil {
					log.Printf("upload error %s: %v", t.E.PathRel, err)
				}
			}
			errCh <- nil
		}()
	}

	for _, e := range en {
		if e.IsDir { continue }
		if e.PathRel == "" || e.PathRel == "." { continue }
		if s.filter != nil && !s.filter.ShouldSync(e.PathRel, false) { continue }
		if isInternalConflictFile(e.PathRel) { continue }
		if s.isRecent(e.PathRel) { continue }
		
		s.trackChecked(e.PathRel)
		
		prev, ok := s.lastLocal[e.PathRel]
		if ok && e.Mtime == prev.Mtime && e.Size == prev.Size {
			continue
		}
		
		jobs <- upTask{E: e}
	}
	
	close(jobs)
	wg.Wait()
	close(errCh)
	
	newMap := map[string]scan.Entry{}
	for _, e := range en { newMap[e.PathRel] = e }
	s.lastLocal = newMap
	
	return nil
}

func (s *Syncer) uploadWorker(ctx context.Context, e scan.Entry) error {
	rel := "/" + e.PathRel
	lp := filepath.Join(s.cfg.LocalPath, filepath.FromSlash(e.PathRel))

	h, _ := scan.HashFile(lp)
	etagToUse := ""
	
	dbOld, _ := store.GetByPathFull(ctx, s.db, e.PathRel)
	
	if dbOld != nil {
		if h == dbOld.Shasum {
			return nil
		}
		etagToUse = dbOld.ETag
	}

	doUpload := func(targetRel, ifMatch string) (*graph.DriveItem, error) {
		if e.Size <= 4*1024*1024 {
			return s.g.UploadSmall(ctx, targetRel, lp, ifMatch)
		}
		return s.g.UploadLarge(ctx, targetRel, lp, ifMatch, s.cfg.UploadChunkMB, s.cfg.UploadParallel)
	}

	it, err := doUpload(rel, etagToUse)

	if err != nil && strings.Contains(err.Error(), "http 412") {
		choice := ui.KeepBoth
		if s.cfg.Interactive {
			choice = ui.ResolveConflict(s.loader, e.PathRel, "Upload")
		}

		switch choice {
		case ui.UseCloud:
			log.Printf("CONFLICT [Revert to Cloud]: %s", e.PathRel)
			s.dbMu.Lock()
			_ = store.DeleteByPath(ctx, s.db, e.PathRel)
			s.dbMu.Unlock()
			return nil
			
		case ui.UseLocal:
			log.Printf("CONFLICT [Force Overwrite Cloud]: %s", e.PathRel)
			it, err = doUpload(rel, "*") 
			
		case ui.KeepBoth:
			conflictPath := "/" + s.makeConflictName(e.PathRel, "local")
			log.Printf("CONFLICT [Keep Both]: Uploading as %s", conflictPath)
			it, err = doUpload(conflictPath, "")
		}
	}

	if err != nil {
		return err
	}

	s.dbMu.Lock()
	_ = store.UpsertItem(ctx, s.db, store.Item{
		ID: it.ID, PathRel: e.PathRel, ETag: it.ETag, Size: it.Size, Mtime: e.Mtime,
		Shasum: h, LastSrc: "local", LastSync: time.Now().Unix(),
	})
	s.dbMu.Unlock()
	
	s.setRecently(e.PathRel, 60*time.Second)
	s.trackChange("upload", e.PathRel, e.Size)
	return nil
}

func (s *Syncer) saveDeltaLink(ctx context.Context, link string) {
	s.dbMu.Lock()
	_ = store.SetMeta(ctx, s.db, "delta_link", link)
	s.dbMu.Unlock()
}

func (s *Syncer) itemPathRel(it graph.DriveItem) string {
	pp := ""
	if it.ParentReference != nil {
		pp = it.ParentReference.Path
	}
	pp = strings.TrimPrefix(pp, "/drive/root:")
	return strings.TrimPrefix(filepath.ToSlash(filepath.Join(pp, it.Name)), "/")
}

func (s *Syncer) makeConflictPath(localPath, tag string) string {
	ext := filepath.Ext(localPath)
	base := strings.TrimSuffix(filepath.Base(localPath), ext)
	name := fmt.Sprintf("%s.%s-conflict-%s%s", base, tag, time.Now().Format("20060102-150405"), ext)
	return filepath.Join(filepath.Dir(localPath), name)
}

func (s *Syncer) makeConflictName(pathRel, tag string) string {
	ext := filepath.Ext(pathRel)
	base := strings.TrimSuffix(filepath.Base(pathRel), ext)
	return fmt.Sprintf("%s.%s-conflict-%s%s", base, tag, time.Now().Format("20060102-150405"), ext)
}

func isInternalConflictFile(pathRel string) bool {
	return strings.Contains(pathRel, "-conflict-")
}

func (s *Syncer) logChange(action, rel string, size int64) {
	if s.cfg.Log == nil || !s.cfg.Log.Verbose { return }
	log.Printf("[%s] %s (%d bytes)", strings.ToUpper(action), rel, size)
}

func (s *Syncer) trackChange(action, rel string, size int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if strings.Contains(action, "upload") {
		s.uploaded = append(s.uploaded, rel)
	} else if strings.Contains(action, "download") {
		s.downloaded = append(s.downloaded, rel)
	} else {
		s.deleted = append(s.deleted, rel)
	}
	s.logChange(action, rel, size)
}

func (s *Syncer) trackChecked(rel string) {
	if s.cfg.Log != nil && s.cfg.Log.ListChecked {
		log.Printf("[check] %s", rel)
	}
}

func (s *Syncer) printSummary() {
	s.mu.Lock()
	ups, dls, dels := len(s.uploaded), len(s.downloaded), len(s.deleted)
	s.uploaded, s.downloaded, s.deleted = nil, nil, nil
	s.mu.Unlock()

	log.Printf("==== SUMMARY: Up %d, Down %d, Del %d ====", ups, dls, dels)
}

func (s *Syncer) setRecently(rel string, d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.recently[rel] = time.Now().Add(d).Unix()
}

func (s *Syncer) isRecent(rel string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.recently[rel]
	return ok && t > time.Now().Unix()
}

func (s *Syncer) clearRecently(rel string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.recently, rel)
}