package syncer

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/xiaochun-z/driftsync/internal/graph"
	"github.com/xiaochun-z/driftsync/internal/scan"
	"github.com/xiaochun-z/driftsync/internal/store"
	"github.com/xiaochun-z/driftsync/internal/ui"
)

// cloudDelta processes all pending cloud changes via the OneDrive Delta API.
// On the first call (or after a 410 Gone), it performs a full sync and
// reconciles any items present in the DB but absent from the cloud.
func (s *Syncer) cloudDelta(ctx context.Context) error {
	isIncremental := s.deltaLink != ""

	var toDownload []dlTask
	cloudAlive := map[string]struct{}{}

	// Paging loop. On 410 Gone, reset state and retry once as a full sync.
	retried := false
	for {
		d, err := s.g.RootDelta(ctx, s.deltaLink)
		if err != nil {
			var gone *graph.DeltaGoneError
			if errors.As(err, &gone) && !retried {
				log.Printf("[WARN] %v. Resetting to full sync.", err)
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
			s.processDeltaItem(ctx, it, &toDownload, cloudAlive)
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

	// Full sync only: remove local files that the cloud no longer has.
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

// processDeltaItem handles a single item from the Delta API response.
func (s *Syncer) processDeltaItem(ctx context.Context, it graph.DriveItem, toDownload *[]dlTask, cloudAlive map[string]struct{}) {
	// Cloud deletion: deleted items often lack Name/ParentReference, so we
	// recover the path from the DB rather than relying on itemPathRel.
	if it.Deleted != nil {
		dbOld, _ := store.GetByID(ctx, s.db, it.ID)

		pathRel := ""
		if dbOld != nil {
			pathRel = dbOld.PathRel
		} else {
			pathRel = s.itemPathRel(it)
		}
		if pathRel == "" || pathRel == "." || pathRel == "/" {
			return
		}

		localPath := filepath.Join(s.cfg.LocalPath, filepath.FromSlash(pathRel))
		s.handleCloudDelete(ctx, pathRel, localPath, dbOld)
		return
	}

	pathRel := s.itemPathRel(it)
	if pathRel == "" || pathRel == "." || pathRel == "/" {
		return
	}
	localPath := filepath.Join(s.cfg.LocalPath, filepath.FromSlash(pathRel))

	if it.Folder != nil {
		if !s.filter.ShouldSync(pathRel, true) {
			return
		}
		_ = os.MkdirAll(localPath, 0o755)
		cloudAlive[pathRel] = struct{}{}
		return
	}

	if it.File != nil {
		if !s.filter.ShouldSync(pathRel, false) {
			return
		}
		mt, _ := time.Parse(time.RFC3339, it.LastModifiedDateTime)
		sha := ""
		if it.File.Hashes != nil {
			sha = strings.ToLower(it.File.Hashes.Sha256Hash)
		}
		*toDownload = append(*toDownload, dlTask{
			ID: it.ID, PathRel: pathRel, Size: it.Size,
			ETag: it.ETag, ModTime: mt, Sha256: sha,
		})
		cloudAlive[pathRel] = struct{}{}
	}
}

// handleCloudDelete applies a cloud-side deletion to the local filesystem.
func (s *Syncer) handleCloudDelete(ctx context.Context, pathRel, localPath string, dbOld *store.Item) {
	shouldDelete := true
	if _, err := os.Stat(localPath); err == nil {
		localHash, _ := scan.HashFile(localPath)
		isDirty := dbOld == nil || localHash != dbOld.Shasum
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
}

// reconcileMissing removes local files that are tracked in the DB but absent
// from the cloud. Only called during a full (non-incremental) sync, when
// cloudAlive contains a complete picture of what the cloud has.
func (s *Syncer) reconcileMissing(ctx context.Context, cloudAlive map[string]struct{}) error {
	dbPaths, err := store.ListAllPaths(ctx, s.db)
	if err != nil {
		return err
	}
	for _, p := range dbPaths {
		if p == "" || p == "." {
			continue
		}
		if !s.filter.ShouldSync(p, false) {
			continue
		}
		if _, ok := cloudAlive[p]; ok {
			continue
		}
		// File is in DB but not in cloud → cloud deleted it.
		lp := filepath.Join(s.cfg.LocalPath, filepath.FromSlash(p))
		if _, statErr := os.Stat(lp); statErr == nil {
			dbOld, _ := store.GetByPathFull(ctx, s.db, p)
			localHash, _ := scan.HashFile(lp)
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
	return nil
}

// processDownloads fans out download tasks across worker goroutines and
// returns an aggregate error if any downloads failed.
func (s *Syncer) processDownloads(ctx context.Context, tasks []dlTask) error {
	workers := s.cfg.DownloadWorkers
	if workers < 1 {
		workers = defaultDownloadWorkers
	}

	jobs := make(chan dlTask, len(tasks))
	var (
		wg    sync.WaitGroup
		errMu sync.Mutex
		errs  []error
	)
	for i := 0; i < workers; i++ {
		wg.Go(func() {
			for t := range jobs {
				if err := s.downloadWorker(ctx, t); err != nil {
					log.Printf("download error [%s]: %v", t.PathRel, err)
					errMu.Lock()
					errs = append(errs, err)
					errMu.Unlock()
				}
			}
		})
	}
	for _, t := range tasks {
		jobs <- t
	}
	close(jobs)
	wg.Wait()

	if len(errs) > 0 {
		return fmt.Errorf("%d file(s) failed to download; last: %w", len(errs), errs[len(errs)-1])
	}
	return nil
}

// downloadWorker resolves conflicts and downloads a single file from the cloud.
func (s *Syncer) downloadWorker(ctx context.Context, t dlTask) error {
	lp := filepath.Join(s.cfg.LocalPath, filepath.FromSlash(t.PathRel))
	if err := os.MkdirAll(filepath.Dir(lp), 0o755); err != nil {
		return err
	}

	dbOld, _ := store.GetByPathFull(ctx, s.db, t.PathRel)

	localHash, localExists := s.localHash(lp, dbOld)

	// Conflict detection
	conflict, localChanged, cloudChanged := s.detectConflict(localExists, localHash, dbOld, t.ETag, t.Sha256)

	// Echo suppression: both sides match DB → already in sync.
	if localExists && dbOld != nil && !localChanged && !cloudChanged {
		return nil
	}

	targetPath := lp
	if conflict {
		choice := ui.KeepBoth
		if s.cfg.Interactive {
			choice = ui.ResolveConflict(s.loader, t.PathRel, "Download")
		}
		switch choice {
		case ui.UseLocal:
			log.Printf("CONFLICT [Keep Local]: skipping download for %s", t.PathRel)
			return nil
		case ui.UseCloud:
			log.Printf("CONFLICT [Overwrite]: downloading cloud version to %s", t.PathRel)
		case ui.KeepBoth:
			targetPath = makeConflictPath(lp, "cloud")
			log.Printf("CONFLICT [Keep Both]: downloading to %s", targetPath)
		}
	} else if localChanged && !cloudChanged {
		// Local is newer; let localScanAndUpload handle the upload.
		log.Printf("[Sync] Skipping download for %s: local file is newer.", t.PathRel)
		return nil
	}

	// Smart sync: content already matches (e.g. metadata-only cloud update).
	if localExists && t.Sha256 != "" && localHash == t.Sha256 {
		log.Printf("[SMART] Content match (metadata update only): %s", t.PathRel)
		s.dbMu.Lock()
		_ = store.UpsertItem(ctx, s.db, store.Item{
			ID: t.ID, PathRel: t.PathRel, ETag: t.ETag, Size: t.Size,
			Shasum: localHash, LastSrc: "cloud", LastSync: time.Now().Unix(),
		})
		s.dbMu.Unlock()
		return nil
	}

	if err := s.g.DownloadTo(ctx, t.ID, targetPath); err != nil {
		return err
	}

	// Post-download integrity check.
	newHash, _ := scan.HashFile(targetPath)
	if t.Sha256 != "" && newHash != "" && newHash != t.Sha256 {
		os.Remove(targetPath)
		return fmt.Errorf("integrity check failed for %s: sha256 mismatch", t.PathRel)
	}

	if targetPath == lp {
		s.dbMu.Lock()
		_ = store.UpsertItem(ctx, s.db, store.Item{
			ID: t.ID, PathRel: t.PathRel, ETag: t.ETag, Size: t.Size,
			Shasum: newHash, LastSrc: "cloud", LastSync: time.Now().Unix(),
		})
		s.dbMu.Unlock()
	}

	s.setRecently(t.PathRel, recentlyDownloadTTL)
	s.trackChange("download", t.PathRel, t.Size)
	return nil
}

// localHash returns the SHA-256 hash of the local file at lp and whether it exists.
// When dbOld is non-nil and size+mtime match the DB record, the stored hash is
// returned directly to avoid a full re-hash.
func (s *Syncer) localHash(lp string, dbOld *store.Item) (hash string, exists bool) {
	info, err := os.Stat(lp)
	if err != nil {
		return "", false
	}
	if dbOld != nil && info.ModTime().Unix() == dbOld.Mtime && info.Size() == dbOld.Size {
		return dbOld.Shasum, true
	}
	h, _ := scan.HashFile(lp)
	return h, true
}

// detectConflict returns whether there is a conflict and the individual change flags.
func (s *Syncer) detectConflict(localExists bool, localHash string, dbOld *store.Item, etag, remoteSha256 string) (conflict, localChanged, cloudChanged bool) {
	if !localExists {
		return false, false, true // file doesn't exist locally — safe to download
	}
	if dbOld == nil {
		// Local file exists but no DB record: cloud wants to write → conflict.
		return true, false, true
	}
	localChanged = localHash != dbOld.Shasum
	if remoteSha256 != "" {
		cloudChanged = remoteSha256 != dbOld.Shasum
	} else {
		cloudChanged = etag != dbOld.ETag
	}
	conflict = localChanged && cloudChanged
	return
}
