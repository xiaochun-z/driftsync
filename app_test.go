package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/xiaochun-z/driftsync/internal/config"
)

func TestRemoveLocalDataPreservesState(t *testing.T) {
	for _, layout := range []string{"separate", "same", "nested"} {
		t.Run(layout, func(t *testing.T) {
			base := t.TempDir()
			root := filepath.Join(base, "sync")
			workspace := root
			switch layout {
			case "separate":
				workspace = filepath.Join(base, "sync-workspace")
			case "nested":
				workspace = filepath.Join(root, "a", "b")
			}
			for _, dir := range []string{root, workspace} {
				if err := os.MkdirAll(dir, 0700); err != nil {
					t.Fatal(err)
				}
			}
			var statePaths []string
			for _, name := range []string{"config.yaml", "driftsync.db", "driftsync.db-wal", "driftsync.db-shm"} {
				path := filepath.Join(workspace, name)
				if err := os.WriteFile(path, []byte(name), 0600); err != nil {
					t.Fatal(err)
				}
				statePaths = append(statePaths, path)
			}
			dataPath := filepath.Join(root, "local.txt")
			if err := os.WriteFile(dataPath, []byte("local data"), 0600); err != nil {
				t.Fatal(err)
			}
			dirs, err := removeLocalData(root, statePaths)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(dataPath); !os.IsNotExist(err) {
				t.Fatalf("local data remains after cleanup: %v", err)
			}
			for _, path := range statePaths {
				data, err := os.ReadFile(path)
				if err != nil || string(data) != filepath.Base(path) {
					t.Fatalf("state file %q was changed: %q, %v", path, data, err)
				}
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			}
			for _, dir := range dirs {
				if err := os.Remove(dir); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := os.Stat(root); !os.IsNotExist(err) {
				t.Fatalf("sync root remains after cleanup: %v", err)
			}
		})
	}
}

func TestStartOverPreservesStateOnLocalDeleteFailure(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows prevents deleting an open file")
	}
	for _, layout := range []string{"separate", "same", "nested", "nested locked file"} {
		t.Run(layout, func(t *testing.T) {
			testStartOverRetry(t, layout)
		})
	}
}

func testStartOverRetry(t *testing.T, layout string) {
	sandbox := t.TempDir()
	home := filepath.Join(sandbox, "home")
	if err := os.MkdirAll(home, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("USERPROFILE", home)
	t.Setenv("APPDATA", filepath.Join(sandbox, "appdata"))
	t.Setenv("DRIFTSYNC_LOCAL_PATH", "")

	root := filepath.Join(sandbox, "sync")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	a := NewApp()
	workspace := root
	if strings.HasPrefix(layout, "nested") {
		workspace = filepath.Join(root, "aaa-workspace")
		if err := os.MkdirAll(workspace, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if layout != "separate" {
		if err := a.SetWorkspaceLocation(workspace); err != nil {
			t.Fatal(err)
		}
	}
	cfgPath := getConfigPath()
	dbPath := getDBPath()
	if err := (&config.Config{LocalPath: root}).Save(cfgPath); err != nil {
		t.Fatal(err)
	}
	configBytes, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	state := map[string]string{cfgPath: string(configBytes)}
	for _, path := range []string{dbPath, dbPath + "-wal", dbPath + "-shm"} {
		state[path] = "state to preserve"
		if err := os.WriteFile(path, []byte(state[path]), 0600); err != nil {
			t.Fatal(err)
		}
	}
	filePath := filepath.Join(root, "locked.txt")
	if layout == "nested locked file" {
		filePath = filepath.Join(workspace, "locked.txt")
	}
	if err := os.WriteFile(filePath, []byte("local file"), 0600); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(filePath)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()

	if err := a.StartOver(); err == nil || !strings.Contains(err.Error(), root) {
		t.Fatalf("expected deletion error for %q, got %v", root, err)
	}
	for path, want := range state {
		got, err := os.ReadFile(path)
		if err != nil || string(got) != want {
			t.Fatalf("failed reset must preserve %q: got %q, err %v", path, got, err)
		}
	}

	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if err := a.StartOver(); err != nil {
		t.Fatalf("retry after releasing file: %v", err)
	}
	for _, path := range []string{root, cfgPath, dbPath, dbPath + "-wal", dbPath + "-shm"} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("retry must delete %q, stat error: %v", path, err)
		}
	}
}

func TestIsDangerousPathWindowsCaseVariants(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows case-insensitive paths")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{home, wd} {
		for _, variant := range []string{strings.ToLower(path), strings.ToUpper(path)} {
			if !isDangerousPath(variant) {
				t.Errorf("protected path %q bypassed guard as %q", path, variant)
			}
		}
	}
}

func TestIsDangerousPath(t *testing.T) {
	// Empty or relative root-ish
	if !isDangerousPath("") {
		t.Error("expected empty string to be dangerous")
	}
	if !isDangerousPath(".") {
		t.Error("expected '.' to be dangerous")
	}
	if !isDangerousPath("/") {
		t.Error("expected '/' to be dangerous")
	}

	// Current working directory
	wd, err := os.Getwd()
	if err == nil {
		if !isDangerousPath(wd) {
			t.Errorf("expected working directory %q to be dangerous", wd)
		}
	}

	// Home directory
	home, err := os.UserHomeDir()
	if err == nil && home != "" {
		if !isDangerousPath(home) {
			t.Errorf("expected home directory %q to be dangerous", home)
		}
	}

	// System root directory
	if !isDangerousPath("C:\\Windows") && !isDangerousPath("/usr") {
		t.Error("expected system directories to be dangerous")
	}

	// Normal sync folder
	safePath := filepath.Join(os.TempDir(), "driftsync-test-safe-dir-xyz")
	if isDangerousPath(safePath) {
		t.Errorf("expected normal temp subfolder %q to be safe", safePath)
	}
}
