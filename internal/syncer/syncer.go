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
	uploaded   []string
	downloaded []string
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

	if f == nil || !f.HasRules {
		log.Printf("Selective sync: OFF (no rules).")
	} else if cfg.Sync != nil {
		log.Printf("Selective sync (YAML): include=%v exclude=%v", cfg.Sync.Include, cfg.Sync.Exclude)
	} else {
		log.Printf("Selective sync (text list): %s", cfg.SyncListPath)
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
	loader := ui.Start(120 * time.Millisecond)
	defer loader.Stop("")
	if err := s.localDetectAndDeleteCloud(ctx); err != nil {
		log.Printf("local->cloud delete err: %v", err)
	}
	if s.cfg.UploadFromLocal {
		if err := s.localScanAndUpload(ctx); err != nil {
			log.Printf("local upload err: %v", err)
		}
	}
	if s.cfg.DownloadFromCloud {
		if err := s.cloudDelta(ctx); err != nil {
			log.Printf("cloud delta err: %v", err)
		}
	}
	s.printSummary()
	return nil
}

func (s *Syncer) cloudDelta(ctx context.Context) error {
	type dlTask struct {
		ID      string
		PathRel string
		Size    int64
		ETag    string
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
				_ = os.Remove(lp)
				if err := store.DeleteByPath(ctx, s.db, pathRel); err != nil {
					log.Printf("db delete FAIL %s: %v", pathRel, err)
				}
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
				toDownload = append(toDownload, dlTask{ID: it.ID, PathRel: pathRel, Size: it.Size, ETag: it.ETag})
				cloudAlive[pathRel] = struct{}{}
			}
		}
		if d.NextLink != "" {
			s.deltaLink = d.NextLink
			continue
		}
		s.deltaLink = d.DeltaLink
		break
	}

	// Reconcile: DB items missing from cloud inventory are treated as cloud-deleted.
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
				_ = store.DeleteByPath(ctx, s.db, p)
				delete(s.recently, p)
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
				localHash := ""
				if _, err := os.Stat(lp); err == nil {
					if h, err := scan.HashFile(lp); err == nil {
						localHash = h
					}
				}
				dbOld, _ := store.GetByPathFull(ctx, s.db, rel)
				conflict := false
				if dbOld != nil && localHash != "" && dbOld.Sha1 != "" && localHash != dbOld.Sha1 && dbOld.ETag != "" && dbOld.ETag != t.ETag {
					conflict = true
				}
				targetPath := lp
				if conflict {
					ext := filepath.Ext(lp)
					base := strings.TrimSuffix(filepath.Base(lp), ext)
					conflictName := base + ".cloud-conflict-" + time.Now().UTC().Format("20060102-150405") + ext
					targetPath = filepath.Join(filepath.Dir(lp), conflictName)
					log.Printf("CONFLICT: keeping both versions for %s; cloud saved as %s", rel, conflictName)
				}

				existed := (localHash != "") // 下载前是否已有同名本地文件

				if err := s.g.DownloadTo(ctx, t.ID, targetPath); err != nil {
					log.Printf("cloud→local FAIL %s: %v", rel, err)
					continue
				}

				h, herr := scan.HashFile(targetPath)
				if herr != nil {
					h = ""
				}

				// 只有真正改变了本地状态时，才记为“downloaded”
				// 条件：1) 冲突导致另存（targetPath!=lp），或 2) 原先不存在，或 3) 内容确实变化（h != localHash）
				changed := (targetPath != lp) || (!existed) || (existed && h != localHash)

				_ = store.UpsertItem(ctx, s.db, store.Item{
					ID: t.ID, PathRel: rel, ETag: t.ETag, Size: t.Size, Mtime: 0,
					Sha1: h, LastSrc: "cloud", LastSync: time.Now().Unix(),
				})
				s.recently[rel] = time.Now().Add(90 * time.Second).Unix()

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
		if until, ok := s.recently[e.PathRel]; ok && until > time.Now().Unix() {
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
				if old, err := store.GetByPathFull(ctx, s.db, e.PathRel); err == nil {
					if old.LastSrc == "cloud" {
						if h == "" {
							continue
						}

						if old.Sha1 == "" {
							_ = store.UpsertItem(ctx, s.db, store.Item{
								ID:       old.ID,
								PathRel:  old.PathRel,
								ETag:     old.ETag,
								Size:     old.Size,
								Mtime:    old.Mtime,
								Sha1:     h,
								LastSrc:  old.LastSrc,
								LastSync: time.Now().Unix(),
							})
							continue
						}

						if h == old.Sha1 {
							continue
						}
					} else {
						if h != "" && h == old.Sha1 {
							continue
						}
					}
					if old.ETag != "" {
						if it, err := s.g.GetItemByPath(ctx, rel); err == nil && it.ETag != "" && it.ETag != old.ETag && old.Sha1 != "" && h != old.Sha1 {
							conflictRel := "/" + conflictName(e.PathRel, "local-conflict")
							log.Printf("CONFLICT: both changed; uploading local as %s", conflictRel)
							if e.Size <= 4*1024*1024 {
								if _, err := s.g.UploadSmall(ctx, conflictRel, lp); err != nil {
									log.Printf("local→cloud FAIL %s: %v", e.PathRel, err)
									continue
								}
							} else {
								if _, err := s.g.UploadLarge(ctx, conflictRel, lp, s.cfg.UploadChunkMB, s.cfg.UploadParallel); err != nil {
									log.Printf("local→cloud FAIL %s: %v", e.PathRel, err)
									continue
								}
							}
							s.recently[e.PathRel] = time.Now().Add(60 * time.Second).Unix()
							continue
						}
					}
				}

				var it *graph.DriveItem
				var err error
				if e.Size <= 4*1024*1024 {
					it, err = s.g.UploadSmall(ctx, rel, lp)
				} else {
					it, err = s.g.UploadLarge(ctx, rel, lp, s.cfg.UploadChunkMB, s.cfg.UploadParallel)
				}
				if err != nil {
					log.Printf("local→cloud FAIL %s: %v", e.PathRel, err)
					continue
				}
				_ = store.UpsertItem(ctx, s.db, store.Item{
					ID: it.ID, PathRel: e.PathRel, ETag: it.ETag, Size: it.Size, Mtime: e.Mtime,
					Sha1: h, LastSrc: "local", LastSync: time.Now().Unix(),
				})
				s.recently[e.PathRel] = time.Now().Add(60 * time.Second).Unix()
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
		graphPath := "/" + rel

		if err := s.g.DeleteByPath(ctx, graphPath); err != nil {
			log.Printf("cloud delete FAIL %s: %v", rel, err)
			continue
		}
		s.trackDeleted(rel)
		if err := store.DeleteByPath(ctx, s.db, rel); err != nil {
			log.Printf("db delete FAIL %s: %v", rel, err)
		}
		delete(s.recently, rel)
	}
	return nil
}

func conflictName(pathRel, tag string) string {
	ext := filepath.Ext(pathRel)
	base := strings.TrimSuffix(filepath.Base(pathRel), ext)
	name := base + "." + tag + "-" + time.Now().UTC().Format("20060102-150405") + ext
	return strings.TrimPrefix(filepath.Join(filepath.Dir(pathRel), name), "/")
}

func (s *Syncer) logChange(action, rel string, size int64) {
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
