package scan

import (
	"os"
	"path/filepath"
)

type Entry struct {
	PathRel string
	Size    int64
	Mtime   int64
	IsDir   bool
}

func ScanDir(root string) ([]Entry, error) {
	var out []Entry
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			if os.IsPermission(err) {
				// Only return SkipDir if it is actually a directory.
				if d != nil && d.IsDir() {
					return filepath.SkipDir
				}
				return nil // Skip this file, continue walking
			}
			// Robustness: If file vanished during scan (race condition), skip it instead of aborting sync.
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		out = append(out, Entry{
			PathRel: filepath.ToSlash(rel),
			Size:    info.Size(),
			Mtime:   info.ModTime().Unix(),
			IsDir:   d.IsDir(),
		})
		return nil
	})
	return out, err
}
