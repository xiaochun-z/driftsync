package syncer

import (
	"context"
	"database/sql"
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

type Syncer struct {
	cfg        *config.Config
	db         *sql.DB
	g          *graph.Client
	deltaLink  string
	lastLocal  map[string]scan.Entry
	filter     *selective.List
	recently   map[string]int64
	mu         sync.Mutex
	dbMu       sync.Mutex // Serializes DB writes to prevent SQLITE_BUSY
	uploaded   []string
	downloaded []string
	loader     *ui.Loader // Reference to control spinner
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

	if cfg.Log != nil && cfg.Log.Verbose {
		if f == nil || !f.HasRules {
			log.Printf("Selective sync: OFF (no rules).")
		} else if cfg.Sync != nil {
			log.Printf("Selective sync (YAML): include=%v exclude=%v", cfg.Sync.Include, cfg.Sync.Exclude)
		} else {
			log.Printf("Selective sync (text list): %s", cfg.SyncListPath)
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
	}
}

func (s *Syncer) SyncOnce(ctx context.Context) error {
	s.loader = ui.Start(120 * time.Millisecond)
	defer s.loader.Stop("")

	// Ensure local root exists to prevent lstat errors later
	if err := os.MkdirAll(s.cfg.LocalPath, 0o755); err != nil {
		log.Printf("create local root error: %v", err)
		return nil
	}

	// 1. Cloud First: Check for updates or restores on the server side BEFORE scanning local.
	// This prevents local stale files from overwriting a server-side restore/version bump.
	if s.cfg.DownloadFromCloud {
		if s.deltaLink == "" {
			// Try loading from DB
			val, err := store.GetMeta(ctx, s.db, "delta_link")
			if err == nil && val != "" {
				s.deltaLink = val
			} else if err != nil && err != sql.ErrNoRows {
				log.Printf("WARN: failed to load delta_link: %v", err)
			}
		}

		// CRITICAL: If cloud sync fails, we MUST stop.
		// Continuing to step 2 (Delete) would cause data loss because
		// files that failed to download would be perceived as "locally deleted"
		// and then wiped from the cloud.
		if err := s.cloudDelta(ctx); err != nil {
			log.Printf("cloud delta err: %v", err)
			return err
		}
	}

	// 2. Local Deletes: Propagate local deletions to cloud
	if err := s.localDetectAndDeleteCloud(ctx); err != nil {
		log.Printf("local->cloud delete err: %v", err)
	}

	// 3. Local Uploads: Upload new/modified local files
	if s.cfg.UploadFromLocal {
		if err := s.localScanAndUpload(ctx); err != nil {
			log.Printf("local upload err: %v", err)
		}
	}

	s.loader.Stop("")
	s.printSummary()
	return nil
}

func (s *Syncer) cloudDelta(ctx context.Context) error {
	// Capture if we are starting with a delta link (Incremental Sync)
	// If yes, we MUST NOT run the "Reconcile" logic at the end, because the API
	// returns only changes, not the full state.
	isIncremental := s.deltaLink != ""

	type dlTask struct {
		ID      string
		PathRel string
		Size    int64
		ETag    string
		CTag    string
		ModTime time.Time
	}

	var toDownload []dlTask
	cloudAlive := map[string]struct{}{}

	for {
		d, err := s.g.RootDelta(ctx, s.deltaLink)
		if err != nil {
			return err
		}
		for _, it := range d.Value {
			pathRel := s.itemPathRel(it)

			if it.Deleted != nil {
				lp := filepath.Join(s.cfg.LocalPath, filepath.FromSlash(pathRel))
				_ = os.RemoveAll(lp)
				s.dbMu.Lock()
				if err := store.DeleteByPath(ctx, s.db, pathRel); err != nil {
					log.Printf("db delete FAIL %s: %v", pathRel, err)
				}
				s.dbMu.Unlock()
				continue
			}
			if it.Folder != nil {
				if s.filter != nil && !s.filter.ShouldSync(pathRel, true) {
					continue
				}
				lp := filepath.Join(s.cfg.LocalPath, filepath.FromSlash(pathRel))
				_ = os.MkdirAll(lp, 0o755)
				cloudAlive[pathRel] = struct{}{}
				continue
			}
			if it.File != nil {
				if s.filter != nil && !s.filter.ShouldSync(pathRel, false) {
					continue
				}
				// Ignore internal conflict files from cloud (prevent loop)
				if isInternalConflictFile(pathRel) {
					continue
				}

				mt, _ := time.Parse(time.RFC3339, it.LastModifiedDateTime)
				toDownload = append(toDownload, dlTask{ID: it.ID, PathRel: pathRel, Size: it.Size, ETag: it.ETag, CTag: it.CTag, ModTime: mt})
				cloudAlive[pathRel] = struct{}{}
			}
		}
		if d.NextLink != "" {
			s.deltaLink = d.NextLink
			s.dbMu.Lock()
			if err := store.SetMeta(ctx, s.db, "delta_link", s.deltaLink); err != nil {
				log.Printf("WARN: failed to save delta_link (next): %v", err)
			}
			s.dbMu.Unlock()
			continue
		}
		s.deltaLink = d.DeltaLink
		s.dbMu.Lock()
		if err := store.SetMeta(ctx, s.db, "delta_link", s.deltaLink); err != nil {
			log.Printf("WARN: failed to save delta_link (final): %v", err)
		}
		s.dbMu.Unlock()
		break
	}

	if s.cfg.Log != nil && s.cfg.Log.Verbose {
		log.Printf("Cloud inventory: %d items found.", len(cloudAlive))
	}

	// Reconcile: DB items missing from cloud inventory are treated as cloud-deleted.
	// CRITICAL: Only run this during FULL SYNC. In incremental mode, missing items mean "unchanged", not "deleted".
	if !isIncremental {
		dbPaths, err := store.ListAllPaths(ctx, s.db)
		if err == nil {
			for _, p := range dbPaths {
				if s.filter != nil && !s.filter.ShouldSync(p, false) {
					continue
				}
				if _, ok := cloudAlive[p]; !ok {
					lp := filepath.Join(s.cfg.LocalPath, filepath.FromSlash(p))
					if _, statErr := os.Stat(lp); statErr == nil {
						_ = os.Remove(lp)
						s.trackDeleted(p)
					}
					s.dbMu.Lock()
					_ = store.DeleteByPath(ctx, s.db, p)
					s.dbMu.Unlock()
					s.clearRecently(p)
				}
			}
		}
	}

	if len(toDownload) == 0 {
		return nil
	}

	workers := s.cfg.DownloadWorkers
	if workers < 1 {
		workers = 8
	}

	jobs := make(chan dlTask, workers*2)
	done := make(chan struct{})
	errCh := make(chan error, workers)

	for i := 0; i < workers; i++ {
		go func() {
			for t := range jobs {
				rel := t.PathRel
				lp := filepath.Join(s.cfg.LocalPath, filepath.FromSlash(rel))
				if err := os.MkdirAll(filepath.Dir(lp), 0o755); err != nil {
					log.Printf("mkdir: %v", err)
					continue
				}
				dbOld, _ := store.GetByPathFull(ctx, s.db, rel)
				localHash := ""

				if info, err := os.Stat(lp); err == nil {
					// 优化：不要直接比较本地与云端的时间（避免时区/时钟错误导致的误判）。
					// 而是比较“本地文件是否相对于上次同步有变化”。
					// 如果本地文件的时间戳和大小与数据库记录完全一致，则认为内容未变(Clean)，跳过耗时的哈希计算。
					if dbOld != nil && info.ModTime().Unix() == dbOld.Mtime && info.Size() == dbOld.Size {
						localHash = dbOld.Shasum
					} else {
						// 文件属性有变化，或者是新文件，计算真实哈希以确认内容是否改变
						if h, err := scan.HashFile(lp); err == nil {
							localHash = h
						}
					}
				}
				conflict := false
				if dbOld != nil && localHash != "" && dbOld.Shasum != "" && localHash != dbOld.Shasum && dbOld.ETag != "" && dbOld.ETag != t.ETag {
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
						log.Printf("CONFLICT: User selected Local. Skipping download for %s", rel)
						continue // Skip download, keep local
					case ui.UseCloud:
						log.Printf("CONFLICT: User selected Cloud. Overwriting %s", rel)
						// targetPath remains lp, will overwrite
					case ui.KeepBoth:
						ext := filepath.Ext(lp)
						base := strings.TrimSuffix(filepath.Base(lp), ext)
						conflictName := base + ".cloud-conflict-" + time.Now().UTC().Format("20060102-150405") + ext
						targetPath = filepath.Join(filepath.Dir(lp), conflictName)
						log.Printf("CONFLICT: keeping both versions for %s; cloud saved as %s", rel, conflictName)
					}
				}

				existed := (localHash != "") // 下载前是否已有同名本地文件

				if err := s.g.DownloadTo(ctx, t.ID, targetPath); err != nil {
					log.Printf("cloud→local FAIL %s: %v", rel, err)
					continue
				}

				if t.CTag != "" && s.cfg.Log != nil && s.cfg.Log.Verbose {
					log.Printf("  -> Downloaded version: %s", t.CTag)
				}

				h, herr := scan.HashFile(targetPath)
				if herr != nil {
					h = ""
				}

				// 只有真正改变了本地状态时，才记为“downloaded”
				// 条件：1) 冲突导致另存（targetPath!=lp），或 2) 原先不存在，或 3) 内容确实变化（h != localHash）
				changed := (targetPath != lp) || (!existed) || (existed && h != localHash)

				if targetPath == lp {
					s.dbMu.Lock()
					if err := store.UpsertItem(ctx, s.db, store.Item{
						ID: t.ID, PathRel: rel, ETag: t.ETag, Size: t.Size, Mtime: 0,
						Shasum: h, LastSrc: "cloud", LastSync: time.Now().Unix(),
					}); err != nil {
						s.dbMu.Unlock()
						log.Printf("ERR: db write failed for %s: %v", rel, err)
					} else {
						s.dbMu.Unlock()
					}
				}
				s.setRecently(rel, 90*time.Second)

				if changed {
					s.trackDownloaded(rel, t.Size)
				}
			}
			errCh <- nil
		}()
	}

	go func() {
		for _, t := range toDownload {
			jobs <- t
		}
		close(jobs)
		for i := 0; i < workers; i++ {
			<-errCh
		}
		close(done)
	}()

	<-done
	return nil
}

func (s *Syncer) itemPathRel(it graph.DriveItem) string {
	pp := ""
	if it.ParentReference != nil {
		pp = it.ParentReference.Path
	}
	pp = strings.TrimPrefix(pp, "/drive/root:")
	if pp == "" {
		pp = "/"
	}
	if pp == "/" {
		return it.Name
	}
	return strings.TrimPrefix(pp+"/"+it.Name, "/")
}

func (s *Syncer) localScanAndUpload(ctx context.Context) error {
	en, err := scan.ScanDir(s.cfg.LocalPath)
	if err != nil {
		return err
	}

	cur := map[string]scan.Entry{}
	type upTask struct{ E scan.Entry }
	var toUpload []upTask

	for _, e := range en {
		if e.IsDir {
			continue
		}
		if s.filter != nil && !s.filter.ShouldSync(e.PathRel, false) {
			continue
		}
		// Ignore internal conflict files
		if isInternalConflictFile(e.PathRel) {
			continue
		}

		if s.isRecent(e.PathRel) {
			continue
		}
		s.trackChecked(e.PathRel)
		cur[e.PathRel] = e

		prev, ok := s.lastLocal[e.PathRel]
		if ok && e.Mtime == prev.Mtime && e.Size == prev.Size {
			continue
		}
		toUpload = append(toUpload, upTask{E: e})
	}
	if len(toUpload) == 0 {
		s.lastLocal = cur
		return nil
	}

	workers := s.cfg.UploadWorkers
	if workers < 1 {
		workers = 8
	}

	jobs := make(chan upTask, workers*2)
	done := make(chan struct{})
	errCh := make(chan error, workers)

	for i := 0; i < workers; i++ {
		go func() {
			for t := range jobs {
				e := t.E
				rel := "/" + e.PathRel
				lp := filepath.Join(s.cfg.LocalPath, filepath.FromSlash(e.PathRel))

				h, _ := scan.HashFile(lp)
				etagToUse := ""

				// Declare old outside so we can use it in conflict handler
				var old *store.Item
				if item, err := store.GetByPathFull(ctx, s.db, e.PathRel); err == nil {
					old = item
					if old.LastSrc == "cloud" {
						if h == "" {
							continue
						}
						// 首次计算本地Hash，仅更新DB不上传
						if old.Shasum == "" {
							s.dbMu.Lock()
							_ = store.UpsertItem(ctx, s.db, store.Item{
								ID:       old.ID,
								PathRel:  old.PathRel,
								ETag:     old.ETag,
								Size:     old.Size,
								Mtime:    old.Mtime,
								Shasum:   h,
								LastSrc:  old.LastSrc,
								LastSync: time.Now().Unix(),
							})
							s.dbMu.Unlock()
							continue
						}
						// 内容未变
						if h == old.Shasum {
							continue
						}
					} else {
						// LastSrc=local，内容未变
						if h != "" && h == old.Shasum {
							continue
						}
					}
					// 准备使用乐观锁 ETag
					if old.ETag != "" {
						etagToUse = old.ETag
					}
				}

				// 封装上传逻辑以便复用
				doUpload := func(targetRel, ifMatch string) (*graph.DriveItem, error) {
					if e.Size <= 4*1024*1024 {
						return s.g.UploadSmall(ctx, targetRel, lp, ifMatch)
					}
					return s.g.UploadLarge(ctx, targetRel, lp, ifMatch, s.cfg.UploadChunkMB, s.cfg.UploadParallel)
				}

				// 尝试带 ETag 上传
				it, err := doUpload(rel, etagToUse)

				// 捕获 412 冲突 (Precondition Failed)
				if err != nil && strings.Contains(err.Error(), "http 412") {
					choice := ui.KeepBoth
					if s.cfg.Interactive {
						choice = ui.ResolveConflict(s.loader, e.PathRel, "Upload")
					}

					switch choice {
					case ui.UseCloud:
						log.Printf("CONFLICT: User selected Cloud. Reverting to cloud version for %s", e.PathRel)

						// Immediate recovery: Download cloud file to overwrite local
						// 1. Determine Cloud ID
						targetID := ""

						// CRITICAL: Always try to fetch the FRESH ID by path first.
						// The 'old.ID' in DB might be stale (deleted/recreated on server), leading to 404s.
						if ci, err := s.g.GetItemByPath(ctx, "/"+e.PathRel); err == nil {
							targetID = ci.ID
						} else if old != nil && old.ID != "" {
							// Fallback to DB ID only if path lookup fails (e.g. file moved?)
							targetID = old.ID
						}

						if targetID != "" {
							// 2. Download
							if err := s.g.DownloadTo(ctx, targetID, lp); err != nil {
								log.Printf("  FAIL: could not download cloud version: %v", err)
							} else {
								// 3. Update DB to match new file, preventing future "Local is newer" loop
								if newItem, err := s.g.GetItemByPath(ctx, "/"+e.PathRel); err == nil {
									newHash, _ := scan.HashFile(lp)
									s.dbMu.Lock()
									_ = store.UpsertItem(ctx, s.db, store.Item{
										ID:       newItem.ID,
										PathRel:  e.PathRel,
										ETag:     newItem.ETag,
										Size:     newItem.Size,
										Mtime:    0, // Reset mtime so scanner re-checks naturally or 0 to be safe
										Shasum:   newHash,
										LastSrc:  "cloud",
										LastSync: time.Now().Unix(),
									})
									s.dbMu.Unlock()
									s.trackDownloaded(e.PathRel, newItem.Size)
								}
							}
						}

						s.setRecently(e.PathRel, 60*time.Second)
						continue

					case ui.UseLocal:
						log.Printf("CONFLICT: User selected Local. Overwriting Cloud for %s", e.PathRel)

						// Strategy 1: Try forceful overwrite using wildcard ETag "*"
						// This tells the server: "Update this resource regardless of its current version."
						// FIX: Must assign to outer 'it' and 'err', do not use ':=', otherwise results are discarded/shadowed.
						it, err = doUpload(rel, "*")

						// Strategy 2: If Force Overwrite fails (e.g. 412 Strict or 409 Conflict),
						// perform the "Nuclear Option": Delete Cloud File -> Upload as New.
						if err != nil {
							log.Printf("  Force overwrite failed (%v). Switching to Delete+Upload strategy...", err)

							// 1. Delete the stubborn cloud file
							if delErr := s.g.DeleteByPath(ctx, rel); delErr != nil {
								log.Printf("  WARN: Failed to delete cloud file %s: %v", e.PathRel, delErr)
							}

							// 1.5 WAIT for consistency. OneDrive deletions are eventually consistent.
							// Without this sleep, the immediate upload often hits a "ghost" file and returns 412 again.
							time.Sleep(3 * time.Second)

							// 2. Upload as a fresh file (empty ETag)
							// BUG FIX: Capture the new item (it2) so we don't upsert nil/stale 'it' into DB later.
							if it2, err2 := doUpload(rel, ""); err2 != nil {
								log.Printf("local→cloud NUCLEAR upload FAIL %s: %v", e.PathRel, err2)
							} else {
								it = it2  // Update 'it' to the successfully uploaded item
								err = nil // Clear error so we proceed to DB update
							}
						}

						// If err is nil (Strategy 1 or 2 succeeded), the code below falls through
						// to update the local DB with the new cloud metadata.

					case ui.KeepBoth:
						conflictRel := "/" + conflictName(e.PathRel, "local-conflict")
						log.Printf("CONFLICT: cloud changed (412); uploading local as %s", conflictRel)
						if _, err := doUpload(conflictRel, ""); err != nil {
							log.Printf("local→cloud conflict upload FAIL %s: %v", e.PathRel, err)
						}
						s.setRecently(e.PathRel, 60*time.Second)
						continue
					}

					// If we chose UseLocal and cleared the error, we fall through to the DB update logic below.
					// If err is still set (from failed force upload), standard error logging handles it.
				}

				if err != nil {
					log.Printf("local→cloud FAIL %s: %v", e.PathRel, err)
					continue
				}
				s.dbMu.Lock()
				if err := store.UpsertItem(ctx, s.db, store.Item{
					ID: it.ID, PathRel: e.PathRel, ETag: it.ETag, Size: it.Size, Mtime: e.Mtime,
					Shasum: h, LastSrc: "local", LastSync: time.Now().Unix(),
				}); err != nil {
					log.Printf("ERR: db write failed for %s: %v", e.PathRel, err)
				}
				s.dbMu.Unlock()
				s.setRecently(e.PathRel, 60*time.Second)
				s.trackUploaded(e.PathRel, e.Size)
			}
			errCh <- nil
		}()
	}

	go func() {
		for _, t := range toUpload {
			jobs <- t
		}
		close(jobs)
		for i := 0; i < workers; i++ {
			<-errCh
		}
		close(done)
	}()

	<-done
	s.lastLocal = cur
	return nil
}

func (s *Syncer) localDetectAndDeleteCloud(ctx context.Context) error {
	cur := map[string]struct{}{}
	en, err := scan.ScanDir(s.cfg.LocalPath)
	if err != nil {
		return err
	}
	for _, e := range en {
		if e.IsDir {
			continue
		}
		if s.filter != nil && !s.filter.ShouldSync(e.PathRel, false) {
			continue
		}
		// Ignore internal conflict files
		if isInternalConflictFile(e.PathRel) {
			continue
		}
		cur[e.PathRel] = struct{}{}
	}

	rows, err := s.db.QueryContext(ctx, `SELECT path_rel FROM items`)
	if err != nil {
		return err
	}
	defer rows.Close()

	var toDelete []string
	for rows.Next() {
		var pathRel string
		if err := rows.Scan(&pathRel); err != nil {
			return err
		}
		if s.filter != nil && !s.filter.ShouldSync(pathRel, false) {
			continue
		}
		if _, ok := cur[pathRel]; !ok {
			toDelete = append(toDelete, pathRel)
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}

	for _, rel := range toDelete {
		// SAFETY CHECK: Verify if file exists but was skipped by scanner (e.g. permission error).
		// If os.Stat succeeds (err==nil) or fails with permission/other error (not IsNotExist),
		// we MUST NOT delete the cloud copy.
		localPath := filepath.Join(s.cfg.LocalPath, filepath.FromSlash(rel))
		if _, err := os.Stat(localPath); err == nil {
			log.Printf("SAFETY: Skipping delete for %s: file exists locally but was not scanned (permissions?)", rel)
			continue
		} else if !os.IsNotExist(err) {
			log.Printf("SAFETY: Skipping delete for %s: stat error: %v", rel, err)
			continue
		}

		graphPath := "/" + rel

		if err := s.g.DeleteByPath(ctx, graphPath); err != nil {
			log.Printf("cloud delete FAIL %s: %v", rel, err)
			continue
		}
		s.trackDeleted(rel)
		s.dbMu.Lock()
		if err := store.DeleteByPath(ctx, s.db, rel); err != nil {
			log.Printf("db delete FAIL %s: %v", rel, err)
		}
		s.dbMu.Unlock()
		s.clearRecently(rel)
	}
	return nil
}

func conflictName(pathRel, tag string) string {
	ext := filepath.Ext(pathRel)
	base := strings.TrimSuffix(filepath.Base(pathRel), ext)
	name := base + "." + tag + "-" + time.Now().UTC().Format("20060102-150405") + ext
	
	// Construct OS-specific path first
	fullOSPath := filepath.Join(filepath.Dir(pathRel), name)
	
	// Normalize to forward slashes for Graph API
	normalized := filepath.ToSlash(fullOSPath)
	
	// Remove leading slash if present (ensure relative path consistency)
	return strings.TrimPrefix(normalized, "/")
}

func isInternalConflictFile(pathRel string) bool {
	return strings.Contains(pathRel, ".cloud-conflict-") || strings.Contains(pathRel, ".local-conflict-")
}

func (s *Syncer) logChange(action, rel string, size int64) {
	if s.cfg.Log == nil || !s.cfg.Log.Verbose {
		return
	}
	switch action {
	case "upload":
		log.Printf("[UPLOAD] %s (%d bytes)", rel, size)
	case "download":
		log.Printf("[DOWNLOAD] %s (%d bytes)", rel, size)
	case "delete":
		log.Printf("[DELETE] %s", rel)
	default:
		log.Printf("[%s] %s", strings.ToUpper(action), rel)
	}
}

// 只在需要时打印检查日志；不计入最终清单
func (s *Syncer) trackChecked(rel string) {
	if s.cfg.Log != nil && s.cfg.Log.ListChecked {
		log.Printf("[check] %s", rel)
	}
}

// 成功上传后记录
func (s *Syncer) trackUploaded(rel string, size int64) {
	s.mu.Lock()
	s.uploaded = append(s.uploaded, rel)
	s.mu.Unlock()
	s.logChange("upload", rel, size)
}

// 成功下载后记录
func (s *Syncer) trackDownloaded(rel string, size int64) {
	s.mu.Lock()
	s.downloaded = append(s.downloaded, rel)
	s.mu.Unlock()
	s.logChange("download", rel, size)
}

func (s *Syncer) trackDeleted(rel string) {
	s.logChange("delete", rel, 0)
}

// 在 SyncOnce 末尾调用：仅汇总“真正修改过”的文件
func (s *Syncer) printSummary() {
	// 快照一下，避免持锁打印
	s.mu.Lock()
	ups := append([]string(nil), s.uploaded...)
	dls := append([]string(nil), s.downloaded...)
	// 清空，为下一轮做准备（可选）
	s.uploaded = s.uploaded[:0]
	s.downloaded = s.downloaded[:0]
	s.mu.Unlock()

	log.Printf("==== SUMMARY ====")

	if len(ups) == 0 && len(dls) == 0 {
		log.Printf("No file changes this round.")
		return
	}
	if len(ups) > 0 {
		log.Printf("Uploaded (%d):", len(ups))
		for _, p := range ups {
			log.Printf("  %s", p)
		}
	}
	if len(dls) > 0 {
		log.Printf("Downloaded (%d):", len(dls))
		for _, p := range dls {
			log.Printf("  %s", p)
		}
	}
}

func (s *Syncer) setRecently(rel string, d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.recently[rel] = time.Now().Add(d).Unix()
}

func (s *Syncer) isRecent(rel string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	until, ok := s.recently[rel]
	return ok && until > time.Now().Unix()
}

func (s *Syncer) clearRecently(rel string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.recently, rel)
}
