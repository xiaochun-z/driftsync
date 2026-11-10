package graph

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/xiaochun-z/driftsync/internal/scan"
)

func TestDownloadPreservesUserTemporaryNames(t *testing.T) {
	var base string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/bytes" {
			fmt.Fprint(w, "cloud")
		} else {
			fmt.Fprintf(w, `{"@microsoft.graph.downloadUrl":%q}`, base+"/bytes")
		}
	}))
	defer server.Close()
	base = server.URL
	c := NewClient(server.Client())
	c.Base = base
	root := t.TempDir()
	dest := filepath.Join(root, "report.txt")
	for _, suffix := range []string{".tmp", ".old.tmp"} {
		if err := os.WriteFile(dest+suffix, []byte("user data"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.DownloadTo(context.Background(), "id", dest); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{".tmp", ".old.tmp"} {
		if data, err := os.ReadFile(dest + suffix); err != nil || string(data) != "user data" {
			t.Fatalf("lost %s: %q %v", suffix, data, err)
		}
	}
	if matches, _ := filepath.Glob(filepath.Join(root, ".driftsync-tmp-*")); len(matches) != 0 {
		t.Fatalf("leaked temporary files: %v", matches)
	}
	if err := os.WriteFile(filepath.Join(root, ".driftsync-tmp-inflight"), []byte("partial"), 0600); err != nil {
		t.Fatal(err)
	}
	entries, err := scan.ScanDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Fatalf("scanner included in-flight download: %v", entries)
	}
}

func TestDownloadIntegrityFailurePreservesDestination(t *testing.T) {
	var base string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/bytes" {
			fmt.Fprint(w, "corrupt")
		} else {
			fmt.Fprintf(w, `{"@microsoft.graph.downloadUrl":%q}`, base+"/bytes")
		}
	}))
	defer server.Close()
	base = server.URL
	c := NewClient(server.Client())
	c.Base = base
	dest := filepath.Join(t.TempDir(), "a.txt")
	if err := os.WriteFile(dest, []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	expected := fmt.Sprintf("%x", sha256.Sum256([]byte("valid")))
	if err := c.DownloadToChecked(context.Background(), "f", dest, expected, nil); err == nil {
		t.Fatal("expected checksum mismatch")
	}
	if data, err := os.ReadFile(dest); err != nil || string(data) != "original" {
		t.Fatalf("integrity failure removed original: %q %v", data, err)
	}
	if files, _ := filepath.Glob(filepath.Join(filepath.Dir(dest), ".driftsync-tmp-*")); len(files) != 0 {
		t.Fatalf("temporary files leaked: %v", files)
	}
}
