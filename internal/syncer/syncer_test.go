package syncer

import (
	"strings"
	"testing"

	"github.com/xiaochun-z/driftsync/internal/graph"
)

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
	p := makeConflictPath("/sync/notes/today.md", "cloud")
	if p == "/sync/notes/today.md" {
		t.Fatal("conflict path should differ from original")
	}
	if !strings.HasSuffix(p, ".md") {
		t.Fatalf("expected .md suffix, got %q", p)
	}
	if !strings.HasPrefix(p, "/sync/notes/") {
		t.Fatalf("conflict file should stay in same directory, got %q", p)
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
			Name: c.name,
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
