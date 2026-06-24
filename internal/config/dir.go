package config

import (
	"os"
	"path/filepath"
	"strings"
)

// GetAppDir returns the directory where application data and configuration should be stored.
func GetAppDir() string {
	// First, check for workspace pointer in home directory
	home, err := os.UserHomeDir()
	if err == nil {
		ptrPath := filepath.Join(home, ".driftsync", "workspace.txt")
		b, err := os.ReadFile(ptrPath)
		if err == nil {
			customPath := strings.TrimSpace(string(b))
			if info, err := os.Stat(customPath); err == nil && info.IsDir() {
				return customPath
			}
		}
	}

	// Preferred: OS-specific user configuration directory
	configDir, err := os.UserConfigDir()
	if err == nil {
		appDir := filepath.Join(configDir, "driftsync")
		// Ensure the directory exists
		os.MkdirAll(appDir, 0755)
		return appDir
	}

	// Ultimate fallback to portable mode (executable directory)
	exe, err := os.Executable()
	if err != nil {
		return "."
	}
	return filepath.Dir(exe)
}
