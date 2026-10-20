package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	goRuntime "runtime"
	"strings"
	"sync"
	"time"

	"github.com/wailsapp/wails/v2/pkg/runtime"
	"github.com/xiaochun-z/driftsync/internal/auth"
	"github.com/xiaochun-z/driftsync/internal/config"
	"github.com/xiaochun-z/driftsync/internal/graph"
	"github.com/xiaochun-z/driftsync/internal/store"
	"github.com/xiaochun-z/driftsync/internal/syncer"
)

// App struct
type App struct {
	ctx      context.Context
	syncCtx  context.Context
	cancelFn context.CancelFunc
	syncMu   sync.Mutex
	syncWg   sync.WaitGroup
}

// isDangerousPath checks whether a path is root, user home, current directory, or a critical system directory.
func isDangerousPath(p string) bool {
	clean := filepath.Clean(p)
	if clean == "" || clean == "." || clean == "/" || clean == "\\" {
		return true
	}

	// Check volume/drive root (e.g. "C:\", "/", "D:\")
	vol := filepath.VolumeName(clean)
	if vol != "" && (clean == vol || clean == vol+"\\" || clean == vol+"/") {
		return true
	}

	// Check user home directory
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		if samePath(home, clean) {
			return true
		}
	}

	// Check current working directory
	if wd, err := os.Getwd(); err == nil && wd != "" {
		if samePath(wd, clean) {
			return true
		}
	}

	// Check critical system directories
	lower := strings.ToLower(clean)
	systemDirs := []string{
		"c:\\windows", "c:\\program files", "c:\\program files (x86)",
		"/bin", "/sbin", "/usr", "/usr/bin", "/etc", "/var", "/lib", "/sys", "/proc",
		"/system", "/library", "/applications",
	}
	for _, sys := range systemDirs {
		if lower == sys || strings.HasPrefix(lower, sys+"\\") || strings.HasPrefix(lower, sys+"/") {
			return true
		}
	}

	return false
}

// samePath compares cleaned paths using Windows' case-insensitive semantics.
func samePath(a, b string) bool {
	a, b = filepath.Clean(a), filepath.Clean(b)
	if goRuntime.GOOS == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}

// NewApp creates a new App application struct
func NewApp() *App {
	return &App{}
}

// startup is called when the app starts. The context is saved
// so we can call the runtime methods
func (a *App) startup(ctx context.Context) {
	a.ctx = ctx
}

func (a *App) GetAppVersion() string {
	return Version
}

// SetWorkspaceLocation creates or updates the workspace pointer file.
// This allows users to place their configuration in an arbitrary directory.
func (a *App) SetWorkspaceLocation(targetPath string) error {
	absPath, err := filepath.Abs(targetPath)
	if err != nil {
		return err
	}
	if info, err := os.Stat(absPath); err != nil || !info.IsDir() {
		return fmt.Errorf("target is not a valid directory")
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}

	driftsyncDir := filepath.Join(home, ".driftsync")
	if err := os.MkdirAll(driftsyncDir, 0755); err != nil {
		return err
	}

	ptrPath := filepath.Join(driftsyncDir, "workspace.txt")
	return os.WriteFile(ptrPath, []byte(absPath), 0644)
}

func getConfigPath() string {
	return filepath.Join(config.GetAppDir(), "config.yaml")
}

func getDBPath() string {
	return filepath.Join(config.GetAppDir(), "driftsync.db")
}

func (a *App) GetConfig() (*config.Config, error) {
	cfgPath := getConfigPath()
	cfg, err := config.Load(cfgPath)
	if err != nil {
		// Return and automatically save default config if it fails to load
		defCfg := config.FromEnvFallback()
		defCfg.IsFirstRun = true
		_ = defCfg.Save(cfgPath)
		return defCfg, nil
	}
	return cfg, nil
}

func (a *App) SaveConfig(cfg *config.Config) error {
	if err := cfg.Validate(); err != nil {
		return err
	}

	oldCfg, _ := config.Load(getConfigPath())

	err := cfg.Save(getConfigPath())
	if err != nil {
		return err
	}

	if oldCfg != nil && oldCfg.Sync != nil && cfg.Sync != nil {
		oldEx := strings.Join(oldCfg.Sync.Exclude, "|")
		newEx := strings.Join(cfg.Sync.Exclude, "|")
		if oldEx != newEx {
			db, err := store.Open(getDBPath())
			if err == nil {
				ctx := a.ctx
				if ctx == nil {
					ctx = context.Background()
				}
				_ = store.SetMeta(ctx, db, "delta_link", "")
				db.Close()
			}
		}
	}

	return nil
}

func (a *App) SelectDirectory() (string, error) {
	return runtime.OpenDirectoryDialog(a.ctx, runtime.OpenDialogOptions{
		Title: "Select Sync Directory",
	})
}

func (a *App) OpenDirectory(dir string) error {
	absPath, err := filepath.Abs(dir)
	if err != nil {
		return err
	}

	if _, err := os.Stat(absPath); os.IsNotExist(err) {
		res, err := runtime.MessageDialog(a.ctx, runtime.MessageDialogOptions{
			Type:          runtime.QuestionDialog,
			Title:         "Directory Not Found",
			Message:       fmt.Sprintf("The directory '%s' does not exist.\n\nWould you like to create it?", absPath),
			DefaultButton: "Yes",
			CancelButton:  "No",
		})
		if err != nil {
			return err
		}
		if res != "Yes" {
			return nil
		}
		if err := os.MkdirAll(absPath, 0755); err != nil {
			return fmt.Errorf("failed to create directory: %w", err)
		}
	} else if err != nil {
		return err
	}

	var cmd string
	var args []string

	switch goRuntime.GOOS {
	case "windows":
		cmd = "explorer"
		args = []string{absPath}
	case "darwin":
		cmd = "open"
		args = []string{absPath}
	default: // linux, bsd, etc
		cmd = "xdg-open"
		args = []string{absPath}
	}

	return exec.Command(cmd, args...).Start()
}

func (a *App) LocateFile(target string) error {
	var absPath string
	if target == "config.yaml" {
		absPath = getConfigPath()
	} else if target == "driftsync.db" {
		absPath = getDBPath()
	} else {
		var err error
		absPath, err = filepath.Abs(target)
		if err != nil {
			return err
		}
	}

	if _, err := os.Stat(absPath); os.IsNotExist(err) {
		if target == "config.yaml" {
			cfg := config.FromEnvFallback()
			if err := cfg.Save(absPath); err != nil {
				return err
			}
		} else {
			f, err := os.Create(absPath)
			if err != nil {
				return err
			}
			f.Close()
		}
	}

	switch goRuntime.GOOS {
	case "windows":
		return exec.Command("explorer", "/select,", absPath).Start()
	case "darwin":
		return exec.Command("open", "-R", absPath).Start()
	default:
		// Attempt dbus-send on Linux to highlight the file
		err := exec.Command("dbus-send", "--session", "--print-reply", "--dest=org.freedesktop.FileManager1", "--type=method_call", "/org/freedesktop/FileManager1", "org.freedesktop.FileManager1.ShowItems", "array:string:file://"+absPath, "string:").Run()
		if err == nil {
			return nil
		}
		// Fallback: open parent directory
		return exec.Command("xdg-open", filepath.Dir(absPath)).Start()
	}
}

func (a *App) StartSync() error {
	a.syncMu.Lock()
	if a.cancelFn != nil {
		a.syncMu.Unlock()
		return fmt.Errorf("sync is already running")
	}

	parentCtx := a.ctx
	if parentCtx == nil {
		parentCtx = context.Background()
	}
	a.syncCtx, a.cancelFn = context.WithCancel(parentCtx)
	syncCtx := a.syncCtx
	a.syncWg.Add(1)
	a.syncMu.Unlock()

	defer func() {
		a.syncMu.Lock()
		a.cancelFn = nil
		a.syncCtx = nil
		a.syncMu.Unlock()
		a.syncWg.Done()
	}()

	cfgPath := getConfigPath()
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("invalid config: %w", err)
	}

	dbPath := getDBPath()
	db, err := store.Open(dbPath)
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	defer db.Close()

	tokStore := store.NewTokenStore(db)
	authClient := auth.NewDeviceCodeClient(cfg.Tenant, cfg.ClientID, tokStore)

	// Inject Wails event emission for device code
	authClient.OnDeviceCode = func(userCode, verificationURI string) {
		runtime.EventsEmit(a.ctx, "deviceCode", map[string]string{
			"userCode":        userCode,
			"verificationURI": verificationURI,
		})
	}

	httpClient := authClient.AuthorizedClient(syncCtx)
	g := graph.NewClient(httpClient)

	if err := authClient.EnsureLogin(syncCtx); err != nil {
		return fmt.Errorf("login failed: %w", err)
	}

	// Tell frontend login is done
	runtime.EventsEmit(a.ctx, "loginSuccess")

	s := syncer.NewSyncer(cfg, db, g)

	s.OnSyncEvent = func(action, path string) {
		msg := fmt.Sprintf("[%s] %s", strings.ToUpper(action), path)
		runtime.EventsEmit(a.ctx, "syncEvent", msg)
	}

	// Send an event that sync started
	runtime.EventsEmit(a.ctx, "syncStarted")

	if err := s.SyncOnce(syncCtx); err != nil {
		if syncCtx.Err() != nil {
			return fmt.Errorf("sync cancelled")
		}
		return fmt.Errorf("sync error: %w", err)
	}

	runtime.EventsEmit(a.ctx, "syncCompleted")
	return nil
}

func (a *App) StopSync() {
	a.syncMu.Lock()
	defer a.syncMu.Unlock()
	if a.cancelFn != nil {
		a.cancelFn()
	}
}

// GetStartOverPaths returns the absolute paths of the directories and files that will be permanently deleted.
func (a *App) GetStartOverPaths() []string {
	localPath, statePaths := a.startOverPaths()
	if localPath != "" {
		return append([]string{localPath}, statePaths...)
	}
	return statePaths
}

// startOverPaths snapshots all targets before deletion can change the workspace.
// Configuration is removed last so failures can still be retried.
func (a *App) startOverPaths() (string, []string) {
	var localPath string
	cfgPath := getConfigPath()

	cfg, err := config.Load(cfgPath)
	if err == nil && strings.TrimSpace(cfg.LocalPath) != "" {
		localPathAbs, err := filepath.Abs(cfg.LocalPath)
		if err == nil && !isDangerousPath(localPathAbs) {
			localPath = localPathAbs
		}
	}

	dbPath := filepath.Join(filepath.Dir(cfgPath), "driftsync.db")
	dbPathAbs, _ := filepath.Abs(dbPath)
	return localPath, []string{dbPathAbs, dbPathAbs + "-wal", dbPathAbs + "-shm", cfgPath}
}

// removeLocalData leaves state files and their ancestor directories in place.
// The returned directories are ordered from children to parents for removal
// after all local data and state files have been deleted successfully.
func removeLocalData(path string, statePaths []string) ([]string, error) {
	containsState := false
	for _, statePath := range statePaths {
		if samePath(path, statePath) {
			return nil, nil
		}
		rel, err := filepath.Rel(path, statePath)
		if err == nil && !filepath.IsAbs(rel) && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			containsState = true
		}
	}
	if !containsState {
		return nil, os.RemoveAll(path)
	}
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, os.RemoveAll(path)
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return nil, err
	}
	var dirs []string
	for _, entry := range entries {
		childDirs, err := removeLocalData(filepath.Join(path, entry.Name()), statePaths)
		if err != nil {
			return nil, err
		}
		dirs = append(dirs, childDirs...)
	}
	return append(dirs, path), nil
}

// StartOver wipes all user configuration, local files, and the database.
func (a *App) StartOver() error {
	a.StopSync()

	// Wait up to 5 seconds for any running sync process to gracefully exit and release database/file handles
	done := make(chan struct{})
	go func() {
		a.syncWg.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		return fmt.Errorf("timed out waiting for active sync to stop before StartOver")
	}

	localPath, statePaths := a.startOverPaths()
	var remainingDirs []string
	if localPath != "" {
		var err error
		remainingDirs, err = removeLocalData(localPath, statePaths)
		if err != nil {
			return fmt.Errorf("failed to delete %s: %w", localPath, err)
		}
	}
	for _, p := range statePaths {
		if isDangerousPath(p) {
			continue
		}
		if err := os.RemoveAll(p); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("failed to delete %s: %w", p, err)
		}
	}
	for _, dir := range remainingDirs {
		if err := os.Remove(dir); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("failed to delete %s: %w", dir, err)
		}
	}

	return nil
}

// GetRemoteItems fetches the children (files and folders) of a remote directory.
// It handles authentication identically to StartSync.
func (a *App) GetRemoteItems(relPath string) ([]graph.DriveItem, error) {
	cfgPath := getConfigPath()
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return nil, fmt.Errorf("failed to load config: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}

	dbPath := getDBPath()
	db, err := store.Open(dbPath)
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	defer db.Close()

	tokStore := store.NewTokenStore(db)
	authClient := auth.NewDeviceCodeClient(cfg.Tenant, cfg.ClientID, tokStore)

	// Inject Wails event emission for device code
	authClient.OnDeviceCode = func(userCode, verificationURI string) {
		runtime.EventsEmit(a.ctx, "deviceCode", map[string]string{
			"userCode":        userCode,
			"verificationURI": verificationURI,
		})
	}

	httpClient := authClient.AuthorizedClient(a.ctx)
	g := graph.NewClient(httpClient)

	if err := authClient.EnsureLogin(a.ctx); err != nil {
		return nil, fmt.Errorf("login failed: %w", err)
	}

	runtime.EventsEmit(a.ctx, "loginSuccess")

	items, err := g.ListChildren(a.ctx, relPath)
	if err != nil {
		return nil, fmt.Errorf("failed to list children: %w", err)
	}

	return items, nil
}
