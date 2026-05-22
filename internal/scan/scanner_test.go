package scan

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

func TestScanDir_IgnoresTrashAndSystemFiles(t *testing.T) {
	tmp := t.TempDir()

	// 1. 创建正常文件
	os.WriteFile(filepath.Join(tmp, "normal.txt"), []byte("ok"), 0644)

	// 2. 创建垃圾文件 (模拟 .DS_Store)
	os.WriteFile(filepath.Join(tmp, ".DS_Store"), []byte("junk"), 0644)

	// 3. 创建回收站目录 (模拟 Trash Loop 风险)
	trashDir := filepath.Join(tmp, ".driftsync_trash")
	os.Mkdir(trashDir, 0755)
	os.WriteFile(filepath.Join(trashDir, "deleted_file.txt"), []byte("zombie"), 0644)

	// 4. 执行扫描
	entries, err := ScanDir(tmp)
	if err != nil {
		t.Fatalf("ScanDir failed: %v", err)
	}

	// 5. 验证
	foundNormal := false
	for _, e := range entries {
		if e.PathRel == ".DS_Store" {
			t.Fatal("Security Fail: Scanned .DS_Store which should be ignored")
		}
		if e.PathRel == ".driftsync_trash" || e.PathRel == ".driftsync_trash/deleted_file.txt" {
			t.Fatal("Security Fail: Scanned inside trash folder! Risk of infinite upload loop.")
		}
		if e.PathRel == "normal.txt" {
			foundNormal = true
		}
	}

	if !foundNormal {
		t.Fatal("Logic Fail: Did not find normal file")
	}
}

func TestScanDir_IgnoresSystemFiles(t *testing.T) {
	tmp := t.TempDir()
	os.WriteFile(filepath.Join(tmp, "Thumbs.db"), []byte("x"), 0644)
	os.WriteFile(filepath.Join(tmp, "desktop.ini"), []byte("x"), 0644)
	os.WriteFile(filepath.Join(tmp, "keep.txt"), []byte("x"), 0644)

	entries, err := ScanDir(tmp)
	if err != nil {
		t.Fatalf("ScanDir: %v", err)
	}
	for _, e := range entries {
		if e.PathRel == "Thumbs.db" || e.PathRel == "desktop.ini" {
			t.Fatalf("should ignore system file %q", e.PathRel)
		}
	}
}

func TestScanDir_SubdirsIncluded(t *testing.T) {
	tmp := t.TempDir()
	subdir := filepath.Join(tmp, "sub")
	os.Mkdir(subdir, 0755)
	os.WriteFile(filepath.Join(subdir, "file.txt"), []byte("hello"), 0644)

	entries, err := ScanDir(tmp)
	if err != nil {
		t.Fatalf("ScanDir: %v", err)
	}
	found := false
	for _, e := range entries {
		if e.PathRel == "sub/file.txt" {
			found = true
		}
	}
	if !found {
		t.Fatal("expected sub/file.txt in entries")
	}
}

func TestScanDir_EntryFields(t *testing.T) {
	tmp := t.TempDir()
	data := []byte("content")
	os.WriteFile(filepath.Join(tmp, "a.txt"), data, 0644)

	entries, err := ScanDir(tmp)
	if err != nil {
		t.Fatalf("ScanDir: %v", err)
	}
	for _, e := range entries {
		if e.PathRel == "a.txt" {
			if e.Size != int64(len(data)) {
				t.Fatalf("size: got %d, want %d", e.Size, len(data))
			}
			if e.Mtime == 0 {
				t.Fatal("mtime should be non-zero")
			}
			if e.IsDir {
				t.Fatal("a.txt should not be a dir")
			}
			return
		}
	}
	t.Fatal("a.txt not found in entries")
}

func TestHashFile_KnownValue(t *testing.T) {
	tmp := t.TempDir()
	content := []byte("hello world")
	path := filepath.Join(tmp, "test.txt")
	os.WriteFile(path, content, 0644)

	h := sha256.Sum256(content)
	want := hex.EncodeToString(h[:])

	got, err := HashFile(path)
	if err != nil {
		t.Fatalf("HashFile error: %v", err)
	}
	if got != want {
		t.Fatalf("hash mismatch: got %s, want %s", got, want)
	}
}

func TestHashFile_EmptyFile(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "empty.txt")
	os.WriteFile(path, []byte{}, 0644)

	h := sha256.Sum256([]byte{})
	want := hex.EncodeToString(h[:])

	got, err := HashFile(path)
	if err != nil {
		t.Fatalf("HashFile error: %v", err)
	}
	if got != want {
		t.Fatalf("empty hash mismatch: got %s, want %s", got, want)
	}
}

func TestHashFile_NonexistentFile(t *testing.T) {
	_, err := HashFile("/nonexistent/file.txt")
	if err == nil {
		t.Fatal("expected error for nonexistent file")
	}
}