package syncer

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/xiaochun-z/driftsync/internal/config"
	"github.com/xiaochun-z/driftsync/internal/graph"
	"github.com/xiaochun-z/driftsync/internal/store"
)

func TestSyncOnceSafetyAbortStopsSync(t *testing.T) {
	for _, tc := range []struct {
		name    string
		tracked int
		present int
		wantErr string
	}{
		{"empty root", 1, 0, "is empty"},
		{"mass deletion", 30, 2, "mass deletion detected"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			db, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			root := t.TempDir()
			for i := 0; i < tc.tracked; i++ {
				path := fmt.Sprintf("file%d.txt", i)
				if err := store.UpsertItem(ctx, db, store.Item{ID: path, PathRel: path}); err != nil {
					t.Fatal(err)
				}
				if i < tc.present {
					if err := os.WriteFile(filepath.Join(root, path), []byte("local changes"), 0600); err != nil {
						t.Fatal(err)
					}
				}
			}

			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				http.Error(w, "unexpected cloud request", http.StatusBadRequest)
			}))
			defer server.Close()
			g := graph.NewClient(server.Client())
			g.Base = server.URL
			s := NewSyncer(&config.Config{
				LocalPath: root, UploadFromLocal: true, DownloadFromCloud: true,
			}, db, g)

			err = s.SyncOnce(ctx)
			if err == nil || !strings.Contains(err.Error(), "safety abort") || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("expected %q safety abort, got %v", tc.wantErr, err)
			}
			if got := requests.Load(); got != 0 {
				t.Errorf("safety abort should stop cloud operations, got %d requests", got)
			}
			paths, err := store.ListAllPaths(ctx, db)
			if err != nil {
				t.Fatal(err)
			}
			if len(paths) != tc.tracked {
				t.Errorf("safety abort changed tracked records: got %d, want %d", len(paths), tc.tracked)
			}
		})
	}
}

func TestConflictName(t *testing.T) {
	n := makeConflictName("notes/today.md", "local-conflict")
	if n == "notes/today.md" || n == "" {
		t.Fatalf("conflict name should differ and not be empty")
	}
}

func TestMakeConflictName_PreservesExtension(t *testing.T) {
	n := makeConflictName("docs/report.pdf", "cloud")
	if !strings.HasSuffix(n, ".pdf") {
		t.Fatalf("expected .pdf suffix, got %q", n)
	}
	if strings.Contains(n, "report.pdf.") {
		t.Fatalf("extension should not be doubled: %q", n)
	}
}

func TestMakeConflictName_NoExtension(t *testing.T) {
	n := makeConflictName("Makefile", "local")
	if n == "" {
		t.Fatal("conflict name should not be empty")
	}
	if n == "Makefile" {
		t.Fatal("conflict name should differ from original")
	}
}

func TestMakeConflictName_ContainsTag(t *testing.T) {
	n := makeConflictName("notes/a.md", "mytag")
	if !strings.Contains(n, "mytag") {
		t.Fatalf("conflict name should contain tag, got %q", n)
	}
}

func TestMakeConflictPath_DifferentFromOriginal(t *testing.T) {
	orig := filepath.FromSlash("/sync/notes/today.md")
	p := makeConflictPath(orig, "cloud")
	if p == orig {
		t.Fatal("conflict path should differ from original")
	}
	if !strings.HasSuffix(p, ".md") {
		t.Fatalf("expected .md suffix, got %q", p)
	}
	expectedDir := filepath.Dir(orig)
	if !strings.HasPrefix(p, expectedDir) {
		t.Fatalf("conflict file should stay in same directory %q, got %q", expectedDir, p)
	}
}

func TestIsInternalConflictFile(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		{"notes/file-conflict-20240101-120000.md", true},
		{"notes/file.local-conflict-20240101.md", true},
		{"notes/normal.md", false},
		{"conflict/normal.md", false},
		{"notes/file-conflicting.md", false},
	}
	for _, c := range cases {
		if got := isInternalConflictFile(c.path); got != c.want {
			t.Errorf("isInternalConflictFile(%q) = %v, want %v", c.path, got, c.want)
		}
	}
}

func TestItemPathRel(t *testing.T) {
	s := &Syncer{}
	cases := []struct {
		parentPath string
		name       string
		want       string
	}{
		{"/drive/root:/docs", "readme.md", "docs/readme.md"},
		{"/drive/root:", "file.txt", "file.txt"},
		{"/drive/root:/a/b", "c.txt", "a/b/c.txt"},
	}
	for _, c := range cases {
		it := graph.DriveItem{
			Name:            c.name,
			ParentReference: &graph.DriveItemParentReference{Path: c.parentPath},
		}
		got := s.itemPathRel(it)
		if got != c.want {
			t.Errorf("itemPathRel(%q, %q) = %q, want %q", c.parentPath, c.name, got, c.want)
		}
	}
}

func TestItemPathRel_NoParent(t *testing.T) {
	s := &Syncer{}
	it := graph.DriveItem{Name: "orphan.txt"}
	got := s.itemPathRel(it)
	if got != "orphan.txt" {
		t.Fatalf("expected 'orphan.txt', got %q", got)
	}
}

func TestLocalDetectAndDeleteCloud_EmptyLocalSafetyAbort(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "driftsync-test-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tempDir)

	dbPath := filepath.Join(tempDir, "test.db")
	db, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	ctx := context.Background()
	// Insert 5 items into DB
	for i := 1; i <= 5; i++ {
		p := fmt.Sprintf("file%d.txt", i)
		err := store.UpsertItem(ctx, db, store.Item{
			ID: p, PathRel: p, ETag: "etag", Size: 10,
		})
		if err != nil {
			t.Fatal(err)
		}
	}

	// Empty sync root folder
	localRoot := filepath.Join(tempDir, "empty-sync")
	if err := os.MkdirAll(localRoot, 0755); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{
		LocalPath: localRoot,
	}
	s := NewSyncer(cfg, db, nil)

	err = s.localDetectAndDeleteCloud(ctx)
	if err == nil {
		t.Fatal("expected safety abort error when local sync folder is empty, got nil")
	}
	if !strings.Contains(err.Error(), "safety abort") {
		t.Fatalf("expected error to mention 'safety abort', got %v", err)
	}
}

func TestLocalDetectAndDeleteCloud_MassDeletionSafetyAbort(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "driftsync-test-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tempDir)

	dbPath := filepath.Join(tempDir, "test.db")
	db, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	ctx := context.Background()
	localRoot := filepath.Join(tempDir, "sync-dir")
	if err := os.MkdirAll(localRoot, 0755); err != nil {
		t.Fatal(err)
	}

	// Insert 30 items into DB
	for i := 1; i <= 30; i++ {
		p := fmt.Sprintf("file%d.txt", i)
		err := store.UpsertItem(ctx, db, store.Item{
			ID: p, PathRel: p, ETag: "etag", Size: 10,
		})
		if err != nil {
			t.Fatal(err)
		}
	}

	// Locally, only create 2 files (28 files missing out of 30, > 50% and >= 20)
	for i := 1; i <= 2; i++ {
		filePath := filepath.Join(localRoot, fmt.Sprintf("file%d.txt", i))
		if err := os.WriteFile(filePath, []byte("hello"), 0644); err != nil {
			t.Fatal(err)
		}
	}

	cfg := &config.Config{
		LocalPath: localRoot,
	}
	s := NewSyncer(cfg, db, nil)

	err = s.localDetectAndDeleteCloud(ctx)
	if err == nil {
		t.Fatal("expected safety abort error on mass deletion, got nil")
	}
	if !strings.Contains(err.Error(), "mass deletion detected") {
		t.Fatalf("expected mass deletion error message, got %v", err)
	}
}
