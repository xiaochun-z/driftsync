package scan

import (
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