package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestFromEnvFallback_Defaults(t *testing.T) {
	c := FromEnvFallback()
	if c.Tenant != "common" {
		t.Fatalf("expected tenant 'common', got %q", c.Tenant)
	}
	if !c.DownloadFromCloud {
		t.Fatal("expected DownloadFromCloud=true")
	}
	if !c.UploadFromLocal {
		t.Fatal("expected UploadFromLocal=true")
	}
	if c.DownloadWorkers != 8 {
		t.Fatalf("expected DownloadWorkers=8, got %d", c.DownloadWorkers)
	}
}

func TestApplyEnvOverrides(t *testing.T) {
	t.Setenv("DRIFTSYNC_TENANT", "mytenant")
	t.Setenv("DRIFTSYNC_CLIENT_ID", "myclient")
	t.Setenv("DRIFTSYNC_LOCAL_PATH", "/tmp/sync")
	t.Setenv("DRIFTSYNC_DOWNLOAD_WORKERS", "4")
	t.Setenv("DRIFTSYNC_UPLOAD_WORKERS", "3")
	t.Setenv("DRIFTSYNC_UPLOAD_CHUNK_MB", "16")
	t.Setenv("DRIFTSYNC_UPLOAD_PARALLEL", "2")

	c := &Config{}
	c.ApplyEnvOverrides()

	if c.Tenant != "mytenant" {
		t.Fatalf("tenant: got %q", c.Tenant)
	}
	if c.ClientID != "myclient" {
		t.Fatalf("client_id: got %q", c.ClientID)
	}
	if c.LocalPath != "/tmp/sync" {
		t.Fatalf("local_path: got %q", c.LocalPath)
	}
	if c.DownloadWorkers != 4 {
		t.Fatalf("download_workers: got %d", c.DownloadWorkers)
	}
	if c.UploadWorkers != 3 {
		t.Fatalf("upload_workers: got %d", c.UploadWorkers)
	}
	if c.UploadChunkMB != 16 {
		t.Fatalf("upload_chunk_mb: got %d", c.UploadChunkMB)
	}
}

func TestApplyEnvOverrides_InvalidInt(t *testing.T) {
	t.Setenv("DRIFTSYNC_DOWNLOAD_WORKERS", "not-a-number")
	c := &Config{DownloadWorkers: 5}
	c.ApplyEnvOverrides()
	if c.DownloadWorkers != 5 {
		t.Fatalf("invalid int env should not override; got %d", c.DownloadWorkers)
	}
}

func TestValidate_MissingClientID(t *testing.T) {
	c := &Config{LocalPath: "/tmp/sync"}
	if err := c.Validate(); err == nil {
		t.Fatal("expected error for missing client_id")
	}
}

func TestValidate_MissingLocalPath(t *testing.T) {
	c := &Config{ClientID: "abc"}
	if err := c.Validate(); err == nil {
		t.Fatal("expected error for missing local_path")
	}
}

func TestValidate_ClampsWorkers(t *testing.T) {
	c := &Config{ClientID: "abc", LocalPath: "/tmp/sync", UploadParallel: 10, DownloadWorkers: 0, UploadWorkers: -1, UploadChunkMB: 0}
	if err := c.Validate(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.DownloadWorkers != 8 {
		t.Fatalf("DownloadWorkers should default to 8, got %d", c.DownloadWorkers)
	}
	if c.UploadWorkers != 8 {
		t.Fatalf("UploadWorkers should default to 8, got %d", c.UploadWorkers)
	}
	if c.UploadChunkMB != 8 {
		t.Fatalf("UploadChunkMB should default to 8, got %d", c.UploadChunkMB)
	}
	if c.UploadParallel != 4 {
		t.Fatalf("UploadParallel should be clamped to 4, got %d", c.UploadParallel)
	}
}

func TestLoad_ValidYAML(t *testing.T) {
	tmp := t.TempDir()
	cfgPath := filepath.Join(tmp, "config.yaml")
	content := `
tenant: testtenant
client_id: testclient
local_path: /tmp/mypath
download_from_cloud: true
upload_from_local: false
download_workers: 3
`
	if err := os.WriteFile(cfgPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	c, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("Load error: %v", err)
	}
	if c.Tenant != "testtenant" {
		t.Fatalf("tenant: got %q", c.Tenant)
	}
	if c.ClientID != "testclient" {
		t.Fatalf("client_id: got %q", c.ClientID)
	}
	if c.DownloadWorkers != 3 {
		t.Fatalf("download_workers: got %d", c.DownloadWorkers)
	}
	if c.UploadFromLocal {
		t.Fatal("upload_from_local should be false")
	}
}

func TestLoad_MissingFile(t *testing.T) {
	_, err := Load("/nonexistent/path/config.yaml")
	if err == nil {
		t.Fatal("expected error loading nonexistent file")
	}
}

func TestLoad_SyncSection(t *testing.T) {
	tmp := t.TempDir()
	cfgPath := filepath.Join(tmp, "config.yaml")
	content := `
client_id: abc
local_path: /tmp/x
sync:
  include:
    - /docs
  exclude:
    - "*.tmp"
`
	if err := os.WriteFile(cfgPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("Load error: %v", err)
	}
	if c.Sync == nil {
		t.Fatal("expected Sync section to be loaded")
	}
	if len(c.Sync.Include) != 1 || c.Sync.Include[0] != "/docs" {
		t.Fatalf("unexpected include: %v", c.Sync.Include)
	}
	if len(c.Sync.Exclude) != 1 || c.Sync.Exclude[0] != "*.tmp" {
		t.Fatalf("unexpected exclude: %v", c.Sync.Exclude)
	}
}
