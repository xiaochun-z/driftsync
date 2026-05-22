package graph

import (
	"os"
	"path/filepath"
	"testing"
)

func TestEscapePathSegments(t *testing.T) {
	got := escapePathSegments("dir/子/space file.txt")
	want := "/dir/%E5%AD%90/space%20file.txt"
	if got != want {
		t.Fatalf("escapePathSegments got %s want %s", got, want)
	}
}

func TestEscapePathSegments_Empty(t *testing.T) {
	if got := escapePathSegments(""); got != "/" {
		t.Fatalf("empty path: got %q, want %q", got, "/")
	}
}

func TestEscapePathSegments_Root(t *testing.T) {
	if got := escapePathSegments("/"); got != "/" {
		t.Fatalf("root: got %q, want %q", got, "/")
	}
}

func TestEscapePathSegments_Simple(t *testing.T) {
	got := escapePathSegments("docs/readme.md")
	want := "/docs/readme.md"
	if got != want {
		t.Fatalf("simple path: got %q, want %q", got, want)
	}
}

func TestRenameWithRetry_Success(t *testing.T) {
	tmp := t.TempDir()
	src := filepath.Join(tmp, "src.txt")
	dst := filepath.Join(tmp, "dst.txt")

	os.WriteFile(src, []byte("hello"), 0644)

	if err := renameWithRetry(src, dst); err != nil {
		t.Fatalf("renameWithRetry: %v", err)
	}
	if _, err := os.Stat(dst); err != nil {
		t.Fatal("dst should exist after rename")
	}
	if _, err := os.Stat(src); err == nil {
		t.Fatal("src should be gone after rename")
	}
}

func TestRenameWithRetry_OverwritesExisting(t *testing.T) {
	tmp := t.TempDir()
	src := filepath.Join(tmp, "new.txt")
	dst := filepath.Join(tmp, "existing.txt")

	os.WriteFile(src, []byte("new content"), 0644)
	os.WriteFile(dst, []byte("old content"), 0644)

	if err := renameWithRetry(src, dst); err != nil {
		t.Fatalf("renameWithRetry: %v", err)
	}

	data, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read dst: %v", err)
	}
	if string(data) != "new content" {
		t.Fatalf("dst should contain new content, got %q", string(data))
	}
}

func TestRenameWithRetry_SourceMissing(t *testing.T) {
	tmp := t.TempDir()
	err := renameWithRetry(filepath.Join(tmp, "missing.txt"), filepath.Join(tmp, "dst.txt"))
	if err == nil {
		t.Fatal("expected error when source does not exist")
	}
}
