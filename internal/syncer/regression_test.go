package syncer

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xiaochun-z/driftsync/internal/config"
	"github.com/xiaochun-z/driftsync/internal/graph"
	"github.com/xiaochun-z/driftsync/internal/scan"
	"github.com/xiaochun-z/driftsync/internal/store"
)

func regressionSyncer(t *testing.T, handler http.HandlerFunc) *Syncer {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	g := graph.NewClient(server.Client())
	g.Base = server.URL
	root := t.TempDir()
	if _, err := store.BindSyncRoot(context.Background(), db, root); err != nil {
		t.Fatal(err)
	}
	return NewSyncer(&config.Config{LocalPath: root, UploadWorkers: 1, DownloadWorkers: 1}, db, g)
}

func TestBaselineConflictUploadUsesCurrentCloudETag(t *testing.T) {
	var base string
	var uploads atomic.Int32
	s := regressionSyncer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPut:
			if r.Header.Get("If-Match") != "remote-tag" {
				t.Errorf("unconditional overwrite after baseline conflict: %q", r.Header.Get("If-Match"))
			}
			body, err := io.ReadAll(r.Body)
			if err != nil || string(body) != "local" {
				t.Errorf("wrong upload: %q %v", body, err)
			}
			uploads.Add(1)
			w.WriteHeader(201)
			fmt.Fprint(w, `{"id":"f","eTag":"uploaded","size":5}`)
		case r.URL.Path == "/me/drive/root/delta":
			fmt.Fprintf(w, `{"value":[{"id":"f","name":"a.txt","size":5,"eTag":"remote-tag","file":{}}],"@odata.deltaLink":%q}`, base+"/next")
		case r.URL.Path == "/bytes":
			fmt.Fprint(w, "cloud")
		default:
			fmt.Fprintf(w, `{"@microsoft.graph.downloadUrl":%q}`, base+"/bytes")
		}
	})
	base = s.g.Base
	s.cfg.UploadFromLocal = true
	s.cfg.DownloadFromCloud = true
	if err := os.WriteFile(filepath.Join(s.cfg.LocalPath, "a.txt"), []byte("local"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := s.SyncOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if uploads.Load() != 1 {
		t.Fatalf("expected conditional local upload, got %d", uploads.Load())
	}
	copies, _ := filepath.Glob(filepath.Join(s.cfg.LocalPath, "a.cloud-conflict-*.txt"))
	if len(copies) != 1 {
		t.Fatalf("cloud version not preserved: %v", copies)
	}
	if data, err := os.ReadFile(copies[0]); err != nil || string(data) != "cloud" {
		t.Fatalf("wrong preserved bytes: %q %v", data, err)
	}
}

func TestRootChangeBlocksUploadOnlyUntilCloudBaseline(t *testing.T) {
	var requests atomic.Int32
	s := regressionSyncer(t, func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		http.Error(w, "unexpected cloud request", 400)
	})
	if err := store.UpsertItem(context.Background(), s.db, store.Item{ID: "old", PathRel: "a.txt"}); err != nil {
		t.Fatal(err)
	}
	s.cfg.LocalPath = t.TempDir()
	s.cfg.UploadFromLocal = true
	if err := os.WriteFile(filepath.Join(s.cfg.LocalPath, "a.txt"), []byte("new"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := s.SyncOnce(context.Background()); err == nil || !strings.Contains(err.Error(), "baseline") {
		t.Fatalf("expected baseline requirement, got %v", err)
	}
	if requests.Load() != 0 {
		t.Fatal("uploaded without establishing cloud baseline")
	}
	// The guard must survive process restart after the root was already bound.
	s = NewSyncer(s.cfg, s.db, s.g)
	if err := s.SyncOnce(context.Background()); err == nil || !strings.Contains(err.Error(), "baseline") {
		t.Fatalf("restart bypassed baseline requirement: %v", err)
	}
}

func TestUploadConflictCopyDoesNotReassignOriginalID(t *testing.T) {
	ctx := context.Background()
	s := regressionSyncer(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "local-conflict-") {
			http.Error(w, "conflict", 412)
			return
		}
		if !strings.Contains(r.URL.Path, "/docs/") {
			t.Error("conflict copy left its original directory")
		}
		io.Copy(io.Discard, r.Body)
		w.WriteHeader(201)
		fmt.Fprint(w, `{"id":"copy-id","eTag":"copy-tag","size":3}`)
	})
	if err := os.MkdirAll(filepath.Join(s.cfg.LocalPath, "docs"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.cfg.LocalPath, "docs", "a.txt"), []byte("new"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertItem(ctx, s.db, store.Item{ID: "original-id", PathRel: "docs/a.txt", ETag: "old-tag", Shasum: "old-hash"}); err != nil {
		t.Fatal(err)
	}
	if err := s.localScanAndUpload(ctx); err != nil {
		t.Fatal(err)
	}
	it, err := store.GetByPathFull(ctx, s.db, "docs/a.txt")
	if err != nil || it.ID != "original-id" || it.ETag != "old-tag" {
		t.Fatalf("copy hijacked original tracking: %+v %v", it, err)
	}
}

func TestMissingParentFetchedByID(t *testing.T) {
	var base string
	s := regressionSyncer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/me/drive/root/delta":
			fmt.Fprintf(w, `{"value":[{"id":"f","name":"a.txt","size":3,"file":{},"parentReference":{"id":"d"}}],"@odata.deltaLink":%q}`, base+"/next")
		case "/me/drive/items/d":
			fmt.Fprint(w, `{"id":"d","name":"docs","folder":{},"parentReference":{"id":"r"}}`)
		case "/me/drive/items/r":
			fmt.Fprint(w, `{"id":"r","name":"drive","root":{},"folder":{}}`)
		case "/me/drive/items/f":
			fmt.Fprintf(w, `{"@microsoft.graph.downloadUrl":%q}`, base+"/bytes")
		case "/bytes":
			fmt.Fprint(w, "abc")
		default:
			http.Error(w, "unexpected request", 400)
		}
	})
	base = s.g.Base
	if err := s.cloudDelta(context.Background()); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(filepath.Join(s.cfg.LocalPath, "docs", "a.txt")); err != nil || string(data) != "abc" {
		t.Fatalf("missing parent not resolved: %q %v", data, err)
	}
}

func TestInterruptedRenameCleanupResumes(t *testing.T) {
	ctx := context.Background()
	var base string
	s := regressionSyncer(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"value":[{"id":"f","name":"new.txt","size":3,"eTag":"new","file":{}}],"@odata.deltaLink":%q}`, base+"/next")
	})
	base = s.g.Base
	oldPath := filepath.Join(s.cfg.LocalPath, "old.txt")
	newPath := filepath.Join(s.cfg.LocalPath, "new.txt")
	for _, p := range []string{oldPath, newPath} {
		if err := os.WriteFile(p, []byte("abc"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	h, err := scan.HashFile(newPath)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate a restart after new content and its DB record were committed,
	// before the old location was cleaned up or the delta cursor advanced.
	if err := store.UpsertItem(ctx, s.db, store.Item{ID: "f", PathRel: "new.txt", Size: 3, Shasum: h, ETag: "new"}); err != nil {
		t.Fatal(err)
	}
	pending, _ := json.Marshal(map[string]store.Item{"f": {ID: "f", PathRel: "old.txt", Shasum: h}})
	if err := store.SetMeta(ctx, s.db, "pending_renames", string(pending)); err != nil {
		t.Fatal(err)
	}
	if err := s.cloudDelta(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
		t.Fatalf("old path not cleaned up: %v", err)
	}
	if data, err := os.ReadFile(newPath); err != nil || string(data) != "abc" {
		t.Fatalf("new path damaged: %q %v", data, err)
	}
	if _, err := store.GetMeta(ctx, s.db, "pending_renames"); err == nil {
		t.Fatal("completed recovery state retained")
	}
}

func TestCyclicParentReferenceFailsWithoutCheckpoint(t *testing.T) {
	var base string
	s := regressionSyncer(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"value":[{"id":"d","name":"docs","folder":{},"parentReference":{"id":"d"}}],"@odata.deltaLink":%q}`, base+"/next")
	})
	base = s.g.Base
	if err := s.cloudDelta(context.Background()); err == nil {
		t.Fatal("accepted cyclic hierarchy")
	}
	if link, _ := store.GetMeta(context.Background(), s.db, "delta_link"); link != "" {
		t.Fatal("invalid hierarchy consumed cursor")
	}
}

func TestBaselineIdenticalBytesDoNotLeaveConflictCopy(t *testing.T) {
	var base string
	s := regressionSyncer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/bytes" {
			fmt.Fprint(w, "abc")
		} else {
			fmt.Fprintf(w, `{"@microsoft.graph.downloadUrl":%q}`, base+"/bytes")
		}
	})
	base = s.g.Base
	lp := filepath.Join(s.cfg.LocalPath, "a.txt")
	if err := os.WriteFile(lp, []byte("abc"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := s.downloadWorker(context.Background(), dlTask{ID: "f", PathRel: "a.txt", Size: 3}); err != nil {
		t.Fatal(err)
	}
	if copies, _ := filepath.Glob(filepath.Join(s.cfg.LocalPath, "a.cloud-conflict-*.txt")); len(copies) != 0 {
		t.Fatalf("identical bytes left conflict copies: %v", copies)
	}
	if it, err := store.GetByID(context.Background(), s.db, "f"); err != nil || it.PathRel != "a.txt" {
		t.Fatalf("baseline not recorded: %v %v", it, err)
	}
}

func TestUploadFailurePropagatesAndRetriesNextSync(t *testing.T) {
	var attempts atomic.Int32
	s := regressionSyncer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			http.Error(w, "unexpected request", 400)
			return
		}
		if attempts.Add(1) == 1 {
			http.Error(w, "rejected", 400)
			return
		}
		io.Copy(io.Discard, r.Body)
		w.WriteHeader(201)
		fmt.Fprint(w, `{"id":"f","eTag":"uploaded","size":3}`)
	})
	s.cfg.UploadFromLocal = true
	if err := os.WriteFile(filepath.Join(s.cfg.LocalPath, "a.txt"), []byte("abc"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := s.SyncOnce(context.Background()); err == nil {
		t.Fatal("failed upload reported success")
	}
	if err := s.SyncOnce(context.Background()); err != nil {
		t.Fatalf("retry failed: %v", err)
	}
	if attempts.Load() != 2 {
		t.Fatalf("failed file not retried: %d attempts", attempts.Load())
	}
	if err := s.SyncOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if attempts.Load() != 2 {
		t.Fatal("unchanged successful upload repeated")
	}
}

func TestUploadDetectsImmediateSameSecondEdit(t *testing.T) {
	var uploads atomic.Int32
	s := regressionSyncer(t, func(w http.ResponseWriter, r *http.Request) {
		uploads.Add(1)
		io.Copy(io.Discard, r.Body)
		w.WriteHeader(201)
		fmt.Fprint(w, `{"id":"f","eTag":"uploaded","size":3}`)
	})
	lp := filepath.Join(s.cfg.LocalPath, "a.txt")
	tm := time.Unix(1800000000, 100000000)
	for _, contents := range []string{"old", "new"} {
		if err := os.WriteFile(lp, []byte(contents), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(lp, tm, tm); err != nil {
			t.Fatal(err)
		}
		if err := s.localScanAndUpload(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if uploads.Load() != 2 {
		t.Fatalf("rapid edit suppressed: %d uploads", uploads.Load())
	}
}

func TestCancelUploadWithFullQueueReturns(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	entered := make(chan struct{}, 1)
	s := regressionSyncer(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-ctx.Done()
		http.Error(w, "cancelled", 400)
	})
	for i := 0; i < 10; i++ {
		if err := os.WriteFile(filepath.Join(s.cfg.LocalPath, fmt.Sprintf("%02d.txt", i)), []byte("x"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	done := make(chan error, 1)
	go func() { done <- s.localScanAndUpload(ctx) }()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("upload did not start")
	}
	cancel()
	select {
	case err := <-done:
		if err != context.Canceled {
			t.Fatalf("expected cancellation, got %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("producer blocked after worker cancellation")
	}
}

func TestSameSecondEditPreservedDuringCloudConflict(t *testing.T) {
	var base string
	s := regressionSyncer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/bytes" {
			fmt.Fprint(w, "cloud")
		} else {
			fmt.Fprintf(w, `{"@microsoft.graph.downloadUrl":%q}`, base+"/bytes")
		}
	})
	base = s.g.Base
	lp := filepath.Join(s.cfg.LocalPath, "a.txt")
	tm := time.Unix(1800000000, 100000000)
	if err := os.WriteFile(lp, []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(lp, tm, tm); err != nil {
		t.Fatal(err)
	}
	h, err := scan.HashFile(lp)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertItem(context.Background(), s.db, store.Item{ID: "f", PathRel: "a.txt", Size: 3, Mtime: tm.Unix(), Shasum: h, ETag: "old"}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lp, []byte("new"), 0600); err != nil {
		t.Fatal(err)
	}
	tm = tm.Add(100 * time.Millisecond)
	if err := os.Chtimes(lp, tm, tm); err != nil {
		t.Fatal(err)
	}
	if err := s.downloadWorker(context.Background(), dlTask{ID: "f", PathRel: "a.txt", ETag: "changed", Size: 5}); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(lp); err != nil || string(data) != "new" {
		t.Fatalf("local edit overwritten: %q %v", data, err)
	}
	copies, _ := filepath.Glob(filepath.Join(s.cfg.LocalPath, "a.cloud-conflict-*.txt"))
	if len(copies) != 1 {
		t.Fatalf("missing cloud copy: %v", copies)
	}
}

func TestEditDuringDownloadPreventsReplacement(t *testing.T) {
	var base, lp string
	s := regressionSyncer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/bytes" {
			if err := os.WriteFile(lp, []byte("edited during download"), 0600); err != nil {
				t.Error(err)
			}
			fmt.Fprint(w, "cloud")
		} else {
			fmt.Fprintf(w, `{"@microsoft.graph.downloadUrl":%q}`, base+"/bytes")
		}
	})
	base = s.g.Base
	lp = filepath.Join(s.cfg.LocalPath, "a.txt")
	if err := os.WriteFile(lp, []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	h, err := scan.HashFile(lp)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertItem(context.Background(), s.db, store.Item{ID: "f", PathRel: "a.txt", Size: 3, Shasum: h, ETag: "old"}); err != nil {
		t.Fatal(err)
	}
	if err := s.downloadWorker(context.Background(), dlTask{ID: "f", PathRel: "a.txt", ETag: "new", Size: 5}); err == nil {
		t.Fatal("expected concurrent edit error")
	}
	if data, err := os.ReadFile(lp); err != nil || string(data) != "edited during download" {
		t.Fatalf("concurrent edit lost: %q %v", data, err)
	}
}

func TestDeltaHierarchyAndFolderRenameWithoutChildDelta(t *testing.T) {
	ctx := context.Background()
	var base string
	s := regressionSyncer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/me/drive/root/delta":
			fmt.Fprintf(w, `{"value":[{"id":"f","name":"a.txt","size":3,"file":{},"parentReference":{"id":"d"}},{"id":"d","name":"docs","folder":{},"parentReference":{"id":"r"}},{"id":"e","name":"empty","folder":{},"parentReference":{"id":"d"}},{"id":"r","name":"drive","folder":{},"root":{}}],"@odata.deltaLink":%q}`, base+"/renamed")
		case "/renamed":
			fmt.Fprintf(w, `{"value":[{"id":"d","name":"renamed","folder":{},"parentReference":{"id":"r"}}],"@odata.deltaLink":%q}`, base+"/done")
		case "/me/drive/items/f":
			fmt.Fprintf(w, `{"id":"f","name":"a.txt","size":3,"file":{},"parentReference":{"id":"d"},"@microsoft.graph.downloadUrl":%q}`, base+"/bytes")
		case "/bytes":
			fmt.Fprint(w, "abc")
		default:
			http.Error(w, "unexpected "+r.URL.Path, 400)
		}
	})
	base = s.g.Base
	if err := s.cloudDelta(ctx); err != nil {
		t.Fatal(err)
	}
	old := filepath.Join(s.cfg.LocalPath, "docs", "a.txt")
	if data, err := os.ReadFile(old); err != nil || string(data) != "abc" {
		t.Fatalf("parent IDs not resolved: %q %v", data, err)
	}
	if err := s.cloudDelta(ctx); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(filepath.Join(s.cfg.LocalPath, "renamed", "a.txt")); err != nil || string(data) != "abc" {
		t.Fatalf("descendant not relocated: %q %v", data, err)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatalf("old path remains: %v", err)
	}
	if info, err := os.Stat(filepath.Join(s.cfg.LocalPath, "renamed", "empty")); err != nil || !info.IsDir() {
		t.Fatalf("empty descendant directory not relocated: %v", err)
	}
	it, err := store.GetByID(ctx, s.db, "f")
	if err != nil || it.PathRel != "renamed/a.txt" {
		t.Fatalf("wrong DB path: %v %v", it, err)
	}
}

func TestDeltaUsesLastOccurrenceOfItem(t *testing.T) {
	var base string
	s := regressionSyncer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/me/drive/root/delta" {
			fmt.Fprintf(w, `{"value":[{"id":"f","name":"obsolete.txt","file":{}},{"id":"f","name":"current.txt","size":3,"file":{}}],"@odata.deltaLink":%q}`, base+"/next")
		} else if r.URL.Path == "/bytes" {
			fmt.Fprint(w, "abc")
		} else {
			fmt.Fprintf(w, `{"@microsoft.graph.downloadUrl":%q}`, base+"/bytes")
		}
	})
	base = s.g.Base
	if err := s.cloudDelta(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(s.cfg.LocalPath, "obsolete.txt")); !os.IsNotExist(err) {
		t.Fatalf("processed obsolete occurrence: %v", err)
	}
	if _, err := os.Stat(filepath.Join(s.cfg.LocalPath, "current.txt")); err != nil {
		t.Fatal(err)
	}
}

func TestDeltaDownloadFailureRetriesAfterRestart(t *testing.T) {
	ctx := context.Background()
	var base string
	var fail atomic.Bool
	fail.Store(true)
	s := regressionSyncer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/me/drive/root/delta":
			fmt.Fprintf(w, `{"value":[{"id":"f","name":"a.txt","file":{},"size":3}],"@odata.deltaLink":%q}`, base+"/next")
		case "/me/drive/items/f":
			if fail.Load() {
				http.Error(w, "unavailable", 503)
			} else {
				fmt.Fprintf(w, `{"@microsoft.graph.downloadUrl":%q}`, base+"/bytes")
			}
		case "/bytes":
			fmt.Fprint(w, "abc")
		default:
			http.Error(w, "unexpected cursor", 400)
		}
	})
	base = s.g.Base
	if err := s.cloudDelta(ctx); err == nil {
		t.Fatal("expected failure")
	}
	if link, _ := store.GetMeta(ctx, s.db, "delta_link"); link != "" || s.deltaLink != "" {
		t.Fatal("cursor advanced on failure")
	}
	fail.Store(false)
	restarted := NewSyncer(s.cfg, s.db, s.g)
	if err := restarted.cloudDelta(ctx); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(filepath.Join(s.cfg.LocalPath, "a.txt")); err != nil || string(data) != "abc" {
		t.Fatalf("retry lost file: %q %v", data, err)
	}
	if link, _ := store.GetMeta(ctx, s.db, "delta_link"); link != base+"/next" {
		t.Fatalf("cursor not committed: %q", link)
	}
}

func TestDeltaPageFailureRetainsOriginalCursor(t *testing.T) {
	ctx := context.Background()
	var base string
	s := regressionSyncer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/initial" {
			fmt.Fprintf(w, `{"value":[{"id":"f","name":"a.txt","file":{}}],"@odata.nextLink":%q}`, base+"/page2")
		} else {
			http.Error(w, "unavailable", 503)
		}
	})
	base = s.g.Base
	s.deltaLink = base + "/initial"
	if err := store.SetMeta(ctx, s.db, "delta_link", s.deltaLink); err != nil {
		t.Fatal(err)
	}
	if err := s.cloudDelta(ctx); err == nil {
		t.Fatal("expected page failure")
	}
	if link, _ := store.GetMeta(ctx, s.db, "delta_link"); link != base+"/initial" || s.deltaLink != link {
		t.Fatalf("lost initial cursor: %q", link)
	}
}

func TestSwitchRootDoesNotPropagateOldDeletions(t *testing.T) {
	var deletes atomic.Int32
	var base string
	s := regressionSyncer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "DELETE" {
			deletes.Add(1)
			w.WriteHeader(204)
			return
		}
		fmt.Fprintf(w, `{"value":[],"@odata.deltaLink":%q}`, base+"/next")
	})
	base = s.g.Base
	if err := store.UpsertItem(context.Background(), s.db, store.Item{ID: "old", PathRel: "old.txt"}); err != nil {
		t.Fatal(err)
	}
	s.cfg.LocalPath = t.TempDir()
	s.cfg.UploadFromLocal = true
	s.cfg.DownloadFromCloud = true
	// Exclude the unrelated file from upload; its presence used to bypass the empty-root guard.
	if err := os.WriteFile(filepath.Join(s.cfg.LocalPath, "unrelated.txt"), []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	s.cfg.Sync = &config.SelectiveYAML{Exclude: []string{"unrelated.txt"}}
	s = NewSyncer(s.cfg, s.db, s.g)
	if err := s.SyncOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if deletes.Load() != 0 {
		t.Fatal("new root deleted old cloud files")
	}
}
