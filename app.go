package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	goRuntime "runtime"
	"strings"

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
				_ = store.SetMeta(a.ctx, db, "delta_link", "")
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
	if a.cancelFn != nil {
		return fmt.Errorf("sync is already running")
	}

	a.syncCtx, a.cancelFn = context.WithCancel(a.ctx)
	defer func() {
		a.cancelFn = nil
		a.syncCtx = nil
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

	httpClient := authClient.AuthorizedClient(a.syncCtx)
	g := graph.NewClient(httpClient)

	if err := authClient.EnsureLogin(a.syncCtx); err != nil {
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

	if err := s.SyncOnce(a.syncCtx); err != nil {
		if a.syncCtx.Err() != nil {
			return fmt.Errorf("sync cancelled")
		}
		return fmt.Errorf("sync error: %w", err)
	}

	runtime.EventsEmit(a.ctx, "syncCompleted")
	return nil
}

func (a *App) StopSync() {
	if a.cancelFn != nil {
		a.cancelFn()
	}
}

// GetStartOverPaths returns the absolute paths of the directories and files that will be permanently deleted.
func (a *App) GetStartOverPaths() []string {
	paths := []string{}
	cfgPath := getConfigPath()

	cfg, err := config.Load(cfgPath)
	if err == nil && cfg.LocalPath != "" {
		localPathAbs, _ := filepath.Abs(cfg.LocalPath)
		paths = append(paths, localPathAbs)
	}

	paths = append(paths, cfgPath)

	dbPath := getDBPath()
	dbPathAbs, _ := filepath.Abs(dbPath)
	paths = append(paths, dbPathAbs)
	paths = append(paths, dbPathAbs+"-wal")
	paths = append(paths, dbPathAbs+"-shm")

	return paths
}

// StartOver wipes all user configuration, local files, and the database.
func (a *App) StartOver() error {
	a.StopSync()

	for _, p := range a.GetStartOverPaths() {
		_ = os.RemoveAll(p)
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

