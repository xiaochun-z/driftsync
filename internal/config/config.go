package config

import (
	"fmt"
	"os"
	"strconv"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Tenant            string `yaml:"tenant"`
	ClientID          string `yaml:"client_id"`
	LocalPath         string `yaml:"local_path"`
	DownloadFromCloud bool   `yaml:"download_from_cloud"`
	UploadFromLocal   bool   `yaml:"upload_from_local"`
	SyncListPath      string `yaml:"sync_list_path"`
	DownloadWorkers   int    `yaml:"download_workers"`
	UploadWorkers     int    `yaml:"upload_workers"`
	UploadChunkMB     int    `yaml:"upload_chunk_mb"`
	UploadParallel    int    `yaml:"upload_parallel"`
	Interactive       bool   `yaml:"interactive"`

	Sync *SelectiveYAML `yaml:"sync,omitempty"`
	Log  *LogOptions    `yaml:"log,omitempty"`
}

type SelectiveYAML struct {
	Include []string `yaml:"include,omitempty"`
	Exclude []string `yaml:"exclude,omitempty"`
}

type LogOptions struct {
	ListChecked bool `yaml:"list_checked"`
	Verbose     bool `yaml:"verbose"`
}

func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return FromEnvFallback(), err
	}
	var c Config
	if err := yaml.Unmarshal(b, &c); err != nil {
		return FromEnvFallback(), err
	}
	c.ApplyEnvOverrides()
	return &c, nil
}

func FromEnvFallback() *Config {
	c := &Config{
		Tenant:            "common",
		DownloadFromCloud: true,
		UploadFromLocal:   true,
		DownloadWorkers:   8,
		UploadWorkers:     8,
		UploadChunkMB:     8,
		UploadParallel:    2,
	}
	c.ApplyEnvOverrides()
	return c
}

func (c *Config) ApplyEnvOverrides() {
	if v := os.Getenv("DRIFTSYNC_TENANT"); v != "" {
		c.Tenant = v
	}
	if v := os.Getenv("DRIFTSYNC_CLIENT_ID"); v != "" {
		c.ClientID = v
	}
	if v := os.Getenv("DRIFTSYNC_LOCAL_PATH"); v != "" {
		c.LocalPath = v
	}
	if v := os.Getenv("DRIFTSYNC_SYNC_LIST"); v != "" {
		c.SyncListPath = v
	}
	if v := os.Getenv("DRIFTSYNC_DOWNLOAD_WORKERS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.DownloadWorkers = n
		}
	}
	if v := os.Getenv("DRIFTSYNC_UPLOAD_WORKERS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.UploadWorkers = n
		}
	}
	if v := os.Getenv("DRIFTSYNC_UPLOAD_CHUNK_MB"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.UploadChunkMB = n
		}
	}
	if v := os.Getenv("DRIFTSYNC_UPLOAD_PARALLEL"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.UploadParallel = n
		}
	}
}

func (c *Config) Validate() error {
	if c.ClientID == "" {
		return fmt.Errorf("client_id is required")
	}
	if c.LocalPath == "" {
		return fmt.Errorf("local_path is required")
	}
	if c.DownloadWorkers <= 0 {
		c.DownloadWorkers = 8
	}
	if c.UploadWorkers <= 0 {
		c.UploadWorkers = 8
	}
	if c.UploadChunkMB <= 0 {
		c.UploadChunkMB = 8
	}
	if c.UploadParallel <= 0 {
		c.UploadParallel = 2
	}
	if c.UploadParallel > 4 {
		c.UploadParallel = 4
	}
	return nil
}
