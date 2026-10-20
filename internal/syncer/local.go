package syncer

import (
	"context"
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

// localCleanupExcluded scans the DB for files that were previously synced
// but are now excluded by the configuration rules. It moves them to the local
// trash and removes their DB records without deleting them from the cloud.
func (s *Syncer) localCleanupExcluded(ctx context.Context) error {
	dbPaths, err := store.ListAllPaths(ctx, s.db)
	if err != nil {
		return err
	}

	for _, rel := range dbPaths {
		if rel == "" || rel == "." {
			continue
		}
		if !s.filter.ShouldSync(rel, false) {
			lp := filepath.Join(s.cfg.LocalPath, filepath.FromSlash(rel))
			
			// Only attempt trash if the file exists locally
			if _, err := os.Stat(lp); err == nil {
				if trashErr := s.moveToTrash(lp); trashErr != nil {
					log.Printf("trash error for excluded file %s: %v", rel, trashErr)
				} else {
					log.Printf("clean up locally excluded: %s", rel)
					s.trackChange("cleanup (local)", rel, 0)
				}
			}

			s.dbMu.Lock()
			_ = store.DeleteByPath(ctx, s.db, rel)
			s.dbMu.Unlock()
		}
	}
	return nil
}

// localDetectAndDeleteCloud compares the DB with the current local filesystem.
// Files present in the DB but absent locally are deleted from the cloud,
// propagating intentional local deletions to OneDrive.
func (s *Syncer) localDetectAndDeleteCloud(ctx context.Context) error {
	entries, err := scan.ScanDir(s.cfg.LocalPath)
	if err != nil {
		return err
	}
	localFiles := make(map[string]struct{}, len(entries))
	for _, e := range entries {
		if !e.IsDir {
			localFiles[e.PathRel] = struct{}{}
		}
	}

	// Use the store helper instead of inlining the same SQL query.
	dbPaths, err := store.ListAllPaths(ctx, s.db)
	if err != nil {
		return err
	}

	var toDelete []string
	trackedCount := 0
	for _, rel := range dbPaths {
		if rel == "" || rel == "." {
			continue
		}
		if !s.filter.ShouldSync(rel, false) {
			continue
		}
		trackedCount++
		if _, exists := localFiles[rel]; !exists {
			toDelete = append(toDelete, rel)
		}
	}

	if len(toDelete) == 0 {
		return nil
	}

	// SAFETY CIRCUIT BREAKER:
	// 1. If local folder has NO files at all, but DB has tracked files to delete,
	// it usually indicates the sync directory is unmounted, offline, or accidentally empty.
	if len(localFiles) == 0 && len(toDelete) > 0 {
		log.Printf("[SAFETY ABORT] Local sync directory %q is completely empty while %d tracked file(s) exist in DB. Aborting cloud deletion to prevent catastrophic data loss.", s.cfg.LocalPath, len(toDelete))
		return fmt.Errorf("safety abort: local sync directory %q is empty, refusing to delete %d cloud file(s)", s.cfg.LocalPath, len(toDelete))
	}

	// 2. Mass deletion threshold: if deleting >= 20 files AND more than 50% of all tracked files,
	// abort automatic silent mass deletion to protect cloud assets.
	const massDeleteMinCount = 20
	const massDeleteRatio = 0.5
	if trackedCount >= massDeleteMinCount && len(toDelete) >= massDeleteMinCount && float64(len(toDelete))/float64(trackedCount) > massDeleteRatio {
		pct := float64(len(toDelete)) / float64(trackedCount) * 100
		log.Printf("[SAFETY ABORT] Mass deletion detected: attempting to delete %d of %d tracked files (%.1f%%). Exceeds safety threshold. Aborting cloud deletion.",
			len(toDelete), trackedCount, pct)
		return fmt.Errorf("safety abort: mass deletion detected (%d/%d files, %.1f%%), refusing to auto-delete cloud files", len(toDelete), trackedCount, pct)
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
		s.trackChange("delete (local→cloud)", rel, 0)
		s.dbMu.Lock()
		_ = store.DeleteByPath(ctx, s.db, rel)
		s.dbMu.Unlock()
	}
	return nil
}

// localScanAndUpload scans the local directory and uploads files that are new
// or have changed since the last sync cycle.
func (s *Syncer) localScanAndUpload(ctx context.Context) error {
	entries, err := scan.ScanDir(s.cfg.LocalPath)
	if err != nil {
		return err
	}

	workers := s.cfg.UploadWorkers
	if workers < 1 {
		workers = defaultUploadWorkers
	}

	type upTask struct{ E scan.Entry }
	jobs := make(chan upTask, workers*2)
	var wg sync.WaitGroup

	for i := 0; i < workers; i++ {
		wg.Go(func() {
			for t := range jobs {
				if ctx.Err() != nil {
					return
				}
				if err := s.uploadWorker(ctx, t.E); err != nil {
					log.Printf("upload error %s: %v", t.E.PathRel, err)
				}
			}
		})
	}

	for _, e := range entries {
		if ctx.Err() != nil {
			break
		}
		if e.IsDir || e.PathRel == "" || e.PathRel == "." {
			continue
		}
		if !s.filter.ShouldSync(e.PathRel, false) || isInternalConflictFile(e.PathRel) {
			continue
		}
		if s.isRecent(e.PathRel) {
			continue
		}
		s.trackChecked(e.PathRel)

		// Skip if size and mtime match the previous scan (cheap pre-check).
		if prev, ok := s.lastLocal[e.PathRel]; ok && e.Mtime == prev.Mtime && e.Size == prev.Size {
			continue
		}
		jobs <- upTask{E: e}
	}
	close(jobs)
	wg.Wait()

	// Refresh the mtime+size snapshot for the next cycle.
	newMap := make(map[string]scan.Entry, len(entries))
	for _, e := range entries {
		newMap[e.PathRel] = e
	}
	s.lastLocal = newMap
	return nil
}

// uploadWorker hashes a single local file, compares it to the DB record, and
// uploads if changed. Conflict resolution is applied on HTTP 412.
func (s *Syncer) uploadWorker(ctx context.Context, e scan.Entry) error {
	lp := filepath.Join(s.cfg.LocalPath, filepath.FromSlash(e.PathRel))
	h, _ := scan.HashFile(lp)

	dbOld, _ := store.GetByPathFull(ctx, s.db, e.PathRel)
	if dbOld != nil && h == dbOld.Shasum {
		if dbOld.Mtime != e.Mtime {
			s.dbMu.Lock()
			_ = store.UpsertItem(ctx, s.db, store.Item{
				ID: dbOld.ID, PathRel: e.PathRel, ETag: dbOld.ETag, Size: e.Size, Mtime: e.Mtime,
				Shasum: h, LastSrc: dbOld.LastSrc, LastSync: dbOld.LastSync,
			})
			s.dbMu.Unlock()
		}
		return nil // content unchanged
	}

	etagToUse := ""
	if dbOld != nil {
		etagToUse = dbOld.ETag
	}

	doUpload := func(targetRel, ifMatch string) (*graph.DriveItem, error) {
		if e.Size <= smallFileThreshold {
			return s.g.UploadSmall(ctx, targetRel, lp, ifMatch)
		}
		return s.g.UploadLarge(ctx, targetRel, lp, ifMatch, s.cfg.UploadChunkMB, s.cfg.UploadParallel)
	}

	rel := "/" + e.PathRel
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
			conflictRel := "/" + makeConflictName(e.PathRel, "local")
			log.Printf("CONFLICT [Keep Both]: uploading as %s", conflictRel)
			it, err = doUpload(conflictRel, "")
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

	s.setRecently(e.PathRel, recentlyUploadTTL)
	s.trackChange("upload", e.PathRel, e.Size)
	return nil
}
