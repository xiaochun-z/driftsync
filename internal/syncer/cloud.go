package syncer

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
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
	link := s.deltaLink
	var items []graph.DriveItem

	// Paging loop. On 410 Gone, reset state and retry once as a full sync.
	retried := false
	for {
		d, err := s.g.RootDelta(ctx, link)
		if err != nil {
			var gone *graph.DeltaGoneError
			if errors.As(err, &gone) && !retried {
				log.Printf("[WARN] %v. Resetting to full sync.", err)
				retried = true
				link = ""
				isIncremental = false
				toDownload = toDownload[:0]
				cloudAlive = map[string]struct{}{}
				items = nil
				continue
			}
			return err
		}

		items = append(items, d.Value...)

		if d.NextLink != "" {
			link = d.NextLink
			continue
		}
		link = d.DeltaLink
		if link == "" {
			return fmt.Errorf("delta response missing final deltaLink")
		}
		break
	}
	plan, err := s.planDelta(ctx, items, isIncremental)
	if err != nil {
		return err
	}
	for _, it := range plan.items {
		if it.Deleted != nil {
			continue
		}
		if err := s.processDeltaItem(ctx, it, plan.paths[it.ID], &toDownload, cloudAlive); err != nil {
			return err
		}
	}
	if err := s.processDownloads(ctx, toDownload); err != nil {
		return err
	}
	if err := s.cleanupRenamedPaths(ctx, plan); err != nil {
		return err
	}
	// Delete files before folders; non-empty folders must never be trashed.
	sort.SliceStable(plan.items, func(i, j int) bool { return len(plan.paths[plan.items[i].ID]) > len(plan.paths[plan.items[j].ID]) })
	for _, it := range plan.items {
		if it.Deleted == nil {
			continue
		}
		if err := s.processDeltaItem(ctx, it, plan.paths[it.ID], &toDownload, cloudAlive); err != nil {
			return err
		}
	}

	// Full sync only: remove local files that the cloud no longer has.
	if !isIncremental {
		if err := s.reconcileMissing(ctx, cloudAlive); err != nil {
			return fmt.Errorf("reconcile: %w", err)
		}
	}

	if err := ctx.Err(); err != nil {
		return err
	}
	if err := store.CommitCheckpoint(ctx, s.db, link, plan.nodes, plan.deleted, !isIncremental); err != nil {
		return err
	}
	s.deltaLink = link
	return nil
}

// processDeltaItem handles a single item from the Delta API response.
func (s *Syncer) processDeltaItem(ctx context.Context, it graph.DriveItem, pathRel string, toDownload *[]dlTask, cloudAlive map[string]struct{}) error {
	// Cloud deletion: deleted items often lack Name/ParentReference, so we
	// recover the path from the DB rather than relying on itemPathRel.
	if it.Deleted != nil {
		dbOld, err := store.GetByID(ctx, s.db, it.ID)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if dbOld != nil {
			pathRel = dbOld.PathRel
		}
		if pathRel == "" || pathRel == "." || pathRel == "/" {
			return nil
		}

		localPath := filepath.Join(s.cfg.LocalPath, filepath.FromSlash(pathRel))
		return s.handleCloudDelete(ctx, pathRel, localPath, dbOld)
	}

	if pathRel == "" || pathRel == "." || pathRel == "/" {
		return nil
	}
	localPath := filepath.Join(s.cfg.LocalPath, filepath.FromSlash(pathRel))

	if it.Folder != nil {
		if !s.filter.ShouldSync(pathRel, true) {
			return nil
		}
		if err := os.MkdirAll(localPath, 0o755); err != nil {
			return err
		}
		cloudAlive[pathRel] = struct{}{}
		return nil
	}

	if it.File != nil {
		if !s.filter.ShouldSync(pathRel, false) {
			return nil
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
	return nil
}

// handleCloudDelete applies a cloud-side deletion to the local filesystem.
func (s *Syncer) handleCloudDelete(ctx context.Context, pathRel, localPath string, dbOld *store.Item) error {
	shouldDelete := true
	if info, err := os.Stat(localPath); err == nil {
		if info.IsDir() {
			children, err := os.ReadDir(localPath)
			if err != nil {
				return err
			}
			if len(children) != 0 {
				return nil
			}
			return s.moveToTrash(localPath)
		}
		localHash, err := scan.HashFile(localPath)
		if err != nil {
			return err
		}
		isDirty := dbOld == nil || localHash != dbOld.Shasum
		if isDirty {
			log.Printf("[SAFETY] Skipping cloud deletion for %s: local file has modifications.", pathRel)
			shouldDelete = false
			s.dbMu.Lock()
			err := store.DeleteByPath(ctx, s.db, pathRel)
			s.dbMu.Unlock()
			if err != nil {
				return err
			}
		}
	} else if !os.IsNotExist(err) {
		return err
	}

	if shouldDelete {
		if err := s.moveToTrash(localPath); err != nil {
			return fmt.Errorf("trash %s: %w", pathRel, err)
		} else {
			s.trackChange("delete (cloud)", pathRel, 0)
		}
		s.dbMu.Lock()
		err := store.DeleteByPath(ctx, s.db, pathRel)
		s.dbMu.Unlock()
		if err != nil {
			return err
		}
	}
	return nil
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
			dbOld, err := store.GetByPathFull(ctx, s.db, p)
			if err != nil {
				return err
			}
			localHash, err := scan.HashFile(lp)
			if err != nil {
				return err
			}
			if dbOld != nil && localHash != dbOld.Shasum {
				log.Printf("[SAFETY] Reconcile: keeping %s (modified locally)", p)
				s.dbMu.Lock()
				err := store.DeleteByPath(ctx, s.db, p)
				s.dbMu.Unlock()
				if err != nil {
					return err
				}
				continue
			}
			if err := s.moveToTrash(lp); err != nil {
				return err
			}
			s.trackChange("delete (reconcile)", p, 0)
		} else if !os.IsNotExist(statErr) {
			return statErr
		}
		s.dbMu.Lock()
		err := store.DeleteByPath(ctx, s.db, p)
		s.dbMu.Unlock()
		if err != nil {
			return err
		}
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
				if ctx.Err() != nil {
					return
				}
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
	if err := ctx.Err(); err != nil {
		return err
	}

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

	dbOld, err := store.GetByPathFull(ctx, s.db, t.PathRel)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}

	localHash, localExists, err := s.localHash(lp)
	if err != nil {
		return err
	}

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
		var mtime int64
		if fi, err := os.Stat(lp); err == nil {
			mtime = fi.ModTime().Unix()
		}
		s.dbMu.Lock()
		err := store.UpsertItem(ctx, s.db, store.Item{
			ID: t.ID, PathRel: t.PathRel, ETag: t.ETag, Size: t.Size, Mtime: mtime,
			Shasum: localHash, LastSrc: "cloud", LastSync: time.Now().Unix(),
		})
		s.dbMu.Unlock()
		return err
	}

	beforeReplace := func() error {
		if targetPath != lp {
			return nil
		}
		h, exists, err := s.localHash(lp)
		if err != nil {
			return err
		}
		if exists != localExists || h != localHash {
			return fmt.Errorf("local file changed during download: %s", t.PathRel)
		}
		return nil
	}
	if err := s.g.DownloadToChecked(ctx, t.ID, targetPath, t.Sha256, beforeReplace); err != nil {
		return err
	}

	newHash, err := scan.HashFile(targetPath)
	if err != nil {
		return err
	}
	// Re-baselining an existing directory need not leave duplicate conflict
	// copies when Graph omits SHA-256 but the downloaded bytes are identical.
	if targetPath != lp && newHash == localHash {
		if err := os.Remove(targetPath); err != nil {
			return err
		}
		targetPath = lp
	}

	// Preserve cloud modification time on disk if available
	if !t.ModTime.IsZero() {
		_ = os.Chtimes(targetPath, t.ModTime, t.ModTime)
	}

	// Even when preserving the cloud version in a conflict copy, remember its
	// identity, ETag and content hash. A subsequent upload of the original local
	// file must remain conditional on that cloud version, including first sync.
	{
		var mtime int64
		if !t.ModTime.IsZero() {
			mtime = t.ModTime.Unix()
		} else if fi, err := os.Stat(lp); err == nil {
			mtime = fi.ModTime().Unix()
		}
		s.dbMu.Lock()
		err := store.UpsertItem(ctx, s.db, store.Item{
			ID: t.ID, PathRel: t.PathRel, ETag: t.ETag, Size: t.Size, Mtime: mtime,
			Shasum: newHash, LastSrc: "cloud", LastSync: time.Now().Unix(),
		})
		s.dbMu.Unlock()
		if err != nil {
			return err
		}
	}

	s.trackChange("download", t.PathRel, t.Size)
	return nil
}

// localHash returns the SHA-256 hash of the local file at lp and whether it exists.
// Modification timestamps are insufficient evidence that content is unchanged.
func (s *Syncer) localHash(lp string) (hash string, exists bool, err error) {
	_, err = os.Stat(lp)
	if err != nil {
		if os.IsNotExist(err) {
			return "", false, nil
		}
		return "", false, err
	}
	h, err := scan.HashFile(lp)
	return h, true, err
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
