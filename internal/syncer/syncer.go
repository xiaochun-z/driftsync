package syncer

import (
	"context"
	"database/sql"
	"errors"
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
	dbMu       sync.Mutex
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
		cfg:       cfg,
		db:        db,
		g:         g,
		filter:    f,
		lastLocal: map[string]scan.Entry{},
		recently:  map[string]int64{},
	}
}

// SyncOnce executes a full synchronization cycle safely.
func (s *Syncer) SyncOnce(ctx context.Context) error {
	s.loader = ui.Start(120 * time.Millisecond)
	defer s.loader.Stop("")

	if err := os.MkdirAll(s.cfg.LocalPath, 0o755); err != nil {
		return fmt.Errorf("create local root error: %w", err)
	}

	// 1. Local Deletes First: propagate local deletions to cloud before downloading,
	// so the cloud cannot "save" a locally-deleted file by re-downloading it.
	if err := s.localDetectAndDeleteCloud(ctx); err != nil {
		log.Printf("local->cloud delete err: %v", err)
	}

	// 2. Cloud Updates: check for new/changed/deleted items from cloud.
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

	// 3. Local Uploads: push new/modified local files to cloud.
	if s.cfg.UploadFromLocal {
		if err := s.localScanAndUpload(ctx); err != nil {
			log.Printf("local upload err: %v", err)
		}
	}

	s.loader.Stop("")
	s.printSummary()
	return nil
}

// cloudDelta handles incoming changes from the cloud via the Delta API.
func (s *Syncer) cloudDelta(ctx context.Context) error {
	isIncremental := s.deltaLink != ""

	var toDownload []dlTask
	cloudAlive := map[string]struct{}{}

	// Paging loop for Delta API. On 410 Gone, reset and retry as a full sync (once).
	retried := false
	for {
		d, err := s.g.RootDelta(ctx, s.deltaLink)
		if err != nil {
			// 410 Gone: delta link expired. Reset state and perform a full sync.
			var gone *graph.DeltaGoneError
			if errors.As(err, &gone) && !retried {
				log.Printf("[WARN] %v. Discarding stored delta link.", err)
				retried = true
				s.deltaLink = ""
				s.saveDeltaLink(ctx, "")
				isIncremental = false
				toDownload = toDownload[:0]
				cloudAlive = map[string]struct{}{}
				continue
			}
			return err
		}
		for _, it := range d.Value {
			// === HANDLE CLOUD DELETION ===
			// Process deletions before the path check: deleted items often lack
			// Name/ParentReference, so itemPathRel would return empty. Recover the
			// path from the local DB instead.
			if it.Deleted != nil {
				dbOld, _ := store.GetByID(ctx, s.db, it.ID)

				var pathRel string
				if dbOld != nil {
					pathRel = dbOld.PathRel
				} else {
					pathRel = s.itemPathRel(it)
				}

				if pathRel == "" || pathRel == "." || pathRel == "/" {
					continue
				}

				localPath := filepath.Join(s.cfg.LocalPath, filepath.FromSlash(pathRel))

				shouldDelete := true
				if _, err := os.Stat(localPath); err == nil {
					localHash, _ := scan.HashFile(localPath)

					isDirty := false
					if dbOld == nil {
						// Cloud says delete but we have an untracked local file; keep it.
						isDirty = true
					} else if localHash != dbOld.Shasum {
						// Content changed locally since last sync; keep it.
						isDirty = true
					}

					if isDirty {
						log.Printf("[SAFETY] Skipping cloud deletion for %s: local file has modifications.", pathRel)
						shouldDelete = false
						s.dbMu.Lock()
						_ = store.DeleteByPath(ctx, s.db, pathRel)
						s.dbMu.Unlock()
					}
				}

				if shouldDelete {
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

			// === HANDLE FILES (potential download) ===
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

	// === RECONCILE (full sync only) ===
	// Only runs when we have a complete picture of cloud state (non-incremental).
	if !isIncremental {
		if err := s.reconcileMissing(ctx, cloudAlive); err != nil {
			log.Printf("reconcile error: %v", err)
		}
	}

	if len(toDownload) == 0 {
		return nil
	}
	return s.processDownloads(ctx, toDownload)
}

func (s *Syncer) reconcileMissing(ctx context.Context, cloudAlive map[string]struct{}) error {
	dbPaths, err := store.ListAllPaths(ctx, s.db)
	if err != nil {
		return err
	}
	for _, p := range dbPaths {
		if p == "" || p == "." {
			continue
		}
		if s.filter != nil && !s.filter.ShouldSync(p, false) {
			continue
		}
		if _, ok := cloudAlive[p]; !ok {
			// File is in DB but absent from cloud scan → cloud deleted it.
			lp := filepath.Join(s.cfg.LocalPath, filepath.FromSlash(p))

			if _, statErr := os.Stat(lp); statErr == nil {
				dbOld, _ := store.GetByPathFull(ctx, s.db, p)
				localHash, _ := scan.HashFile(lp)

				// If locally modified, keep the file and untrack it from DB.
				if dbOld != nil && localHash != dbOld.Shasum {
					log.Printf("[SAFETY] Reconcile: keeping %s (modified locally)", p)
					s.dbMu.Lock()
					_ = store.DeleteByPath(ctx, s.db, p)
					s.dbMu.Unlock()
					continue
				}

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

// processDownloads fans out download tasks to worker goroutines and collects errors.
func (s *Syncer) processDownloads(ctx context.Context, dlTasks []dlTask) error {
	workers := s.cfg.DownloadWorkers
	if workers < 1 {
		workers = 8
	}

	jobs := make(chan dlTask, len(dlTasks))
	var (
		wg    sync.WaitGroup
		errMu sync.Mutex
		errs  []error
	)

	for i := 0; i < workers; i++ {
		wg.Go(func() {
			for t := range jobs {
				if err := s.downloadWorker(ctx, t.ID, t.PathRel, t.ETag, t.Sha256, t.Size); err != nil {
					log.Printf("download error [%s]: %v", t.PathRel, err)
					errMu.Lock()
					errs = append(errs, err)
					errMu.Unlock()
				}
			}
		})
	}

	for _, t := range dlTasks {
		jobs <- t
	}
	close(jobs)
	wg.Wait()

	if len(errs) > 0 {
		return fmt.Errorf("%d file(s) failed to download; last error: %w", len(errs), errs[len(errs)-1])
	}
	return nil
}

func (s *Syncer) downloadWorker(ctx context.Context, id, rel, etag, remoteSha256 string, size int64) error {
	lp := filepath.Join(s.cfg.LocalPath, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(lp), 0o755); err != nil {
		return err
	}

	dbOld, _ := store.GetByPathFull(ctx, s.db, rel)

	// Determine local state
	localHash := ""
	localExists := false
	if info, err := os.Stat(lp); err == nil {
		localExists = true
		// Optimization: trust DB hash when size+mtime match, avoiding a full re-hash.
		if dbOld != nil && info.ModTime().Unix() == dbOld.Mtime && info.Size() == dbOld.Size {
			localHash = dbOld.Shasum
		} else {
			localHash, _ = scan.HashFile(lp)
		}
	}

	// Conflict detection
	conflict := false
	localChanged := false
	cloudChanged := false

	if localExists && dbOld != nil {
		localChanged = localHash != dbOld.Shasum
		if remoteSha256 != "" {
			cloudChanged = remoteSha256 != dbOld.Shasum
		} else {
			cloudChanged = etag != dbOld.ETag
		}
		if localChanged && cloudChanged {
			conflict = true
		}
	} else if localExists && dbOld == nil {
		// Local file exists but no DB record; cloud wants to overwrite → conflict.
		conflict = true
	}

	// Echo suppression: if neither side changed relative to DB, we are already in sync.
	if localExists && dbOld != nil && !localChanged && !cloudChanged {
		return nil
	}

	targetPath := lp
	if conflict {
		choice := ui.KeepBoth
		if s.cfg.Interactive {
			choice = ui.ResolveConflict(s.loader, rel, "Download")
		}
		switch choice {
		case ui.UseLocal:
			log.Printf("CONFLICT [Keep Local]: skipping download for %s", rel)
			return nil
		case ui.UseCloud:
			log.Printf("CONFLICT [Overwrite]: downloading cloud version to %s", rel)
		case ui.KeepBoth:
			targetPath = makeConflictPath(lp, "cloud")
			log.Printf("CONFLICT [Keep Both]: downloading to %s", targetPath)
		}
	} else if localChanged && !cloudChanged {
		// Local is newer; skip download and let localScanAndUpload handle it.
		log.Printf("[Sync] Skipping download for %s: local file is newer.", rel)
		return nil
	}

	// Smart sync: if content already matches (e.g. metadata-only cloud update), update DB only.
	if localExists && remoteSha256 != "" && localHash == remoteSha256 {
		log.Printf("[SMART] Content match (metadata-only update): %s", rel)
		s.dbMu.Lock()
		_ = store.UpsertItem(ctx, s.db, store.Item{
			ID: id, PathRel: rel, ETag: etag, Size: size,
			Shasum: localHash, LastSrc: "cloud", LastSync: time.Now().Unix(),
		})
		s.dbMu.Unlock()
		return nil
	}

	// Perform download
	if err := s.g.DownloadTo(ctx, id, targetPath); err != nil {
		return err
	}

	// Post-download integrity check: verify SHA-256 against cloud-provided hash.
	// A mismatch indicates network corruption or a truncated transfer.
	newHash, hashErr := scan.HashFile(targetPath)
	if hashErr == nil && remoteSha256 != "" && newHash != remoteSha256 {
		os.Remove(targetPath)
		return fmt.Errorf("integrity check failed for %s: sha256 mismatch (want %.8s…, got %.8s…)", rel, remoteSha256, newHash)
	}

	// Update DB only when writing to the canonical path (not a conflict copy).
	if targetPath == lp {
		s.dbMu.Lock()
		_ = store.UpsertItem(ctx, s.db, store.Item{
			ID: id, PathRel: rel, ETag: etag, Size: size,
			Shasum: newHash, LastSrc: "cloud", LastSync: time.Now().Unix(),
		})
		s.dbMu.Unlock()
	}

	s.setRecently(rel, 30*time.Second)
	s.trackChange("download", rel, size)
	return nil
}

// moveToTrash moves path into .driftsync_trash/ with a timestamped name that
// encodes the original relative path, preventing collisions and aiding recovery.
func (s *Syncer) moveToTrash(path string) error {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return nil
	}
	// Paranoia: never operate inside the trash folder itself.
	if strings.Contains(path, ".driftsync_trash") {
		return nil
	}

	trashDir := filepath.Join(s.cfg.LocalPath, ".driftsync_trash")
	if err := os.MkdirAll(trashDir, 0o755); err != nil {
		return err
	}

	// Encode the original relative path into the trash filename so users can
	// identify and restore files. Millisecond precision avoids collisions when
	// multiple files share the same basename.
	rel, err := filepath.Rel(s.cfg.LocalPath, path)
	if err != nil {
		rel = filepath.Base(path)
	}
	safeName := strings.NewReplacer("/", "_", "\\", "_", ":", "_").Replace(filepath.ToSlash(rel))
	ts := time.Now().Format("20060102-150405.000")
	dest := filepath.Join(trashDir, fmt.Sprintf("%s.%s.deleted", safeName, ts))
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
	if err != nil {
		return err
	}
	defer rows.Close()

	var toDelete []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return err
		}
		if p == "" || p == "." {
			continue
		}
		if s.filter != nil && !s.filter.ShouldSync(p, false) {
			continue
		}
		if _, exists := cur[p]; !exists {
			toDelete = append(toDelete, p)
		}
	}

	for _, rel := range toDelete {
		lp := filepath.Join(s.cfg.LocalPath, filepath.FromSlash(rel))

		// Safety double-check: abort if the file appeared between scan and now.
		if _, err := os.Stat(lp); err == nil {
			log.Printf("[SAFETY] Aborting cloud delete for %s: file exists locally.", rel)
			continue
		} else if !os.IsNotExist(err) {
			log.Printf("[SAFETY] Aborting cloud delete for %s: FS error %v", rel, err)
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
	if err != nil {
		return err
	}

	workers := s.cfg.UploadWorkers
	if workers < 1 {
		workers = 4
	}

	type upTask struct{ E scan.Entry }
	jobs := make(chan upTask, workers*2)
	var wg sync.WaitGroup

	for i := 0; i < workers; i++ {
		wg.Go(func() {
			for t := range jobs {
				if err := s.uploadWorker(ctx, t.E); err != nil {
					log.Printf("upload error %s: %v", t.E.PathRel, err)
				}
			}
		})
	}

	for _, e := range en {
		if e.IsDir {
			continue
		}
		if e.PathRel == "" || e.PathRel == "." {
			continue
		}
		if s.filter != nil && !s.filter.ShouldSync(e.PathRel, false) {
			continue
		}
		if isInternalConflictFile(e.PathRel) {
			continue
		}
		if s.isRecent(e.PathRel) {
			continue
		}

		s.trackChecked(e.PathRel)

		prev, ok := s.lastLocal[e.PathRel]
		if ok && e.Mtime == prev.Mtime && e.Size == prev.Size {
			continue
		}

		jobs <- upTask{E: e}
	}

	close(jobs)
	wg.Wait()

	newMap := map[string]scan.Entry{}
	for _, e := range en {
		newMap[e.PathRel] = e
	}
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
			conflictPath := "/" + makeConflictName(e.PathRel, "local")
			log.Printf("CONFLICT [Keep Both]: uploading as %s", conflictPath)
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

func makeConflictPath(localPath, tag string) string {
	ext := filepath.Ext(localPath)
	base := strings.TrimSuffix(filepath.Base(localPath), ext)
	name := fmt.Sprintf("%s.%s-conflict-%s%s", base, tag, time.Now().Format("20060102-150405"), ext)
	return filepath.Join(filepath.Dir(localPath), name)
}

func makeConflictName(pathRel, tag string) string {
	ext := filepath.Ext(pathRel)
	base := strings.TrimSuffix(filepath.Base(pathRel), ext)
	return fmt.Sprintf("%s.%s-conflict-%s%s", base, tag, time.Now().Format("20060102-150405"), ext)
}

func isInternalConflictFile(pathRel string) bool {
	return strings.Contains(pathRel, "-conflict-")
}

func (s *Syncer) logChange(action, rel string, size int64) {
	if s.cfg.Log == nil || !s.cfg.Log.Verbose {
		return
	}
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
	ups := s.uploaded
	dls := s.downloaded
	dels := s.deleted
	s.uploaded = nil
	s.downloaded = nil
	s.deleted = nil
	s.mu.Unlock()

	if len(ups) > 0 {
		log.Println("☁️  Uploaded:")
		for _, f := range ups {
			log.Printf("   ↑ %s", f)
		}
	}
	if len(dls) > 0 {
		log.Println("⬇️  Downloaded:")
		for _, f := range dls {
			log.Printf("   ↓ %s", f)
		}
	}
	if len(dels) > 0 {
		log.Println("🗑️  Deleted:")
		for _, f := range dels {
			log.Printf("   × %s", f)
		}
	}

	log.Printf("==== SUMMARY: Up %d, Down %d, Del %d ====", len(ups), len(dls), len(dels))
}

func (s *Syncer) setRecently(rel string, d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.recently[rel] = time.Now().Add(d).Unix()
}

// isRecent reports whether rel was synced within its TTL window.
// Expired entries are removed lazily to prevent unbounded map growth.
func (s *Syncer) isRecent(rel string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.recently[rel]
	if !ok {
		return false
	}
	if t <= time.Now().Unix() {
		delete(s.recently, rel) // lazy cleanup
		return false
	}
	return true
}

func (s *Syncer) clearRecently(rel string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.recently, rel)
}
