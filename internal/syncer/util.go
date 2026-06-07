package syncer

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/xiaochun-z/driftsync/internal/graph"
	"github.com/xiaochun-z/driftsync/internal/store"
)

// --- Path helpers ---

// itemPathRel converts a DriveItem's ParentReference + Name into a relative path
// suitable for use as a local filesystem path and DB key.
func (s *Syncer) itemPathRel(it graph.DriveItem) string {
	pp := ""
	if it.ParentReference != nil {
		pp = it.ParentReference.Path
	}
	if pp == "" && it.Name == "root" {
		// Drive root folder itself has no parent path
		return ""
	}
	pp = strings.TrimPrefix(pp, "/drive/root:")
	return strings.TrimPrefix(filepath.ToSlash(filepath.Join(pp, it.Name)), "/")
}

// isInternalConflictFile reports whether path is a conflict copy created by driftsync.
// Conflict copies are never re-uploaded.
func isInternalConflictFile(pathRel string) bool {
	return strings.Contains(pathRel, "-conflict-")
}

// makeConflictPath returns a conflict-copy path in the same directory as localPath.
func makeConflictPath(localPath, tag string) string {
	ext := filepath.Ext(localPath)
	base := strings.TrimSuffix(filepath.Base(localPath), ext)
	name := fmt.Sprintf("%s.%s-conflict-%s%s", base, tag, time.Now().Format("20060102-150405"), ext)
	return filepath.Join(filepath.Dir(localPath), name)
}

// makeConflictName returns a conflict-copy relative path for a given pathRel.
func makeConflictName(pathRel, tag string) string {
	ext := filepath.Ext(pathRel)
	base := strings.TrimSuffix(filepath.Base(pathRel), ext)
	return fmt.Sprintf("%s.%s-conflict-%s%s", base, tag, time.Now().Format("20060102-150405"), ext)
}

// --- Trash ---

// moveToTrash moves path into .driftsync_trash/ with a timestamped name that
// encodes the original relative path, preventing collisions and aiding recovery.
func (s *Syncer) moveToTrash(path string) error {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return nil
	}
	// Never operate inside the trash folder itself.
	if strings.Contains(path, ".driftsync_trash") {
		return nil
	}

	trashDir := filepath.Join(s.cfg.LocalPath, ".driftsync_trash")
	if err := os.MkdirAll(trashDir, 0o755); err != nil {
		return err
	}

	rel, err := filepath.Rel(s.cfg.LocalPath, path)
	if err != nil {
		rel = filepath.Base(path)
	}
	safeName := strings.NewReplacer("/", "_", "\\", "_", ":", "_").Replace(filepath.ToSlash(rel))
	ts := time.Now().Format("20060102-150405.000") // millisecond precision avoids collisions
	dest := filepath.Join(trashDir, fmt.Sprintf("%s.%s.deleted", safeName, ts))
	return os.Rename(path, dest)
}

// --- Delta link persistence ---

func (s *Syncer) saveDeltaLink(ctx context.Context, link string) {
	s.dbMu.Lock()
	_ = store.SetMeta(ctx, s.db, "delta_link", link)
	s.dbMu.Unlock()
}

// --- Recently-synced map ---

func (s *Syncer) setRecently(rel string, d time.Duration) {
	s.mu.Lock()
	s.recently[rel] = time.Now().Add(d).Unix()
	s.mu.Unlock()
}

// isRecent reports whether rel was synced within its TTL.
// Expired entries are removed lazily to prevent unbounded map growth.
func (s *Syncer) isRecent(rel string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.recently[rel]
	if !ok {
		return false
	}
	if t <= time.Now().Unix() {
		delete(s.recently, rel)
		return false
	}
	return true
}

func (s *Syncer) clearRecently(rel string) {
	s.mu.Lock()
	delete(s.recently, rel)
	s.mu.Unlock()
}

// --- Change tracking and summary ---

func (s *Syncer) trackChange(action, rel string, size int64) {
	s.mu.Lock()
	switch {
	case strings.Contains(action, "upload"):
		s.uploaded = append(s.uploaded, rel)
	case strings.Contains(action, "download"):
		s.downloaded = append(s.downloaded, rel)
	default:
		s.deleted = append(s.deleted, rel)
	}
	s.mu.Unlock()

	if s.cfg.Log != nil && s.cfg.Log.Verbose {
		log.Printf("[%s] %s (%d bytes)", strings.ToUpper(action), rel, size)
	}
	
	if s.OnSyncEvent != nil {
		s.OnSyncEvent(action, rel)
	}
}

func (s *Syncer) trackChecked(rel string) {
	if s.cfg.Log != nil && s.cfg.Log.ListChecked {
		log.Printf("[check] %s", rel)
	}
}

func (s *Syncer) printSummary() {
	s.mu.Lock()
	ups, dls, dels := s.uploaded, s.downloaded, s.deleted
	s.uploaded, s.downloaded, s.deleted = nil, nil, nil
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
