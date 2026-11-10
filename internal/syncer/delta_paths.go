package syncer

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/xiaochun-z/driftsync/internal/graph"
	"github.com/xiaochun-z/driftsync/internal/scan"
	"github.com/xiaochun-z/driftsync/internal/store"
)

type deltaPlan struct {
	items    []graph.DriveItem
	paths    map[string]string
	nodes    map[string]store.RemoteNode
	deleted  []string
	oldPaths map[string]store.Item
	oldDirs  []string
}

func remoteNode(it graph.DriveItem) store.RemoteNode {
	n := store.RemoteNode{ID: it.ID, Name: it.Name, IsDir: it.Folder != nil, IsRoot: it.Root != nil}
	if it.ParentReference != nil {
		n.ParentID = it.ParentReference.ID
		n.PathHint = it.ParentReference.Path
	}
	if n.ParentID == "" && n.PathHint == "" && it.Name == "root" && n.IsDir {
		n.IsRoot = true
	}
	return n
}

// Graph delta paths are optional. Resolve parents by ID, including parents that
// occur later in the feed or that have not changed in this delta batch.
func (s *Syncer) planDelta(ctx context.Context, items []graph.DriveItem, incremental bool) (*deltaPlan, error) {
	nodes, err := store.ListRemoteNodes(ctx, s.db)
	if err != nil {
		return nil, err
	}
	oldNodes := make(map[string]store.RemoteNode, len(nodes))
	for id, n := range nodes {
		oldNodes[id] = n
	}
	p := &deltaPlan{paths: map[string]string{}, nodes: map[string]store.RemoteNode{}, oldPaths: map[string]store.Item{}}
	if pending, err := store.GetMeta(ctx, s.db, "pending_renames"); err == nil {
		if err := json.Unmarshal([]byte(pending), &p.oldPaths); err != nil {
			return nil, err
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	movedFolder := false
	last := map[string]int{}
	for i, it := range items {
		if it.ID == "" {
			return nil, fmt.Errorf("delta item missing ID")
		}
		last[it.ID] = i
	}
	for i, it := range items {
		if last[it.ID] != i {
			continue
		}
		p.items = append(p.items, it)
		if it.Deleted != nil {
			p.deleted = append(p.deleted, it.ID)
		} else {
			n := remoteNode(it)
			if old, ok := nodes[it.ID]; ok && n.IsDir && (old.Name != n.Name || old.ParentID != n.ParentID || old.PathHint != n.PathHint) {
				movedFolder = true
			}
			nodes[it.ID] = n
			p.nodes[it.ID] = n
		}
	}
	visiting := map[string]bool{}
	var resolve func(string) (string, error)
	resolve = func(id string) (string, error) {
		if rel, ok := p.paths[id]; ok {
			return rel, nil
		}
		if visiting[id] {
			return "", fmt.Errorf("cyclic cloud parent reference: %s", id)
		}
		visiting[id] = true
		defer delete(visiting, id)
		n, ok := nodes[id]
		if !ok {
			it, err := s.g.GetItemByID(ctx, id)
			if err != nil {
				return "", err
			}
			n = remoteNode(*it)
			nodes[id] = n
			p.nodes[id] = n
		}
		if n.IsRoot {
			p.paths[id] = ""
			return "", nil
		}
		if n.Name == "" || n.Name == "." || n.Name == ".." || strings.ContainsAny(n.Name, "/\\:") {
			return "", fmt.Errorf("unsafe cloud item name %q", n.Name)
		}
		parent := ""
		if n.ParentID != "" {
			var err error
			parent, err = resolve(n.ParentID)
			if err != nil {
				return "", err
			}
		} else if n.PathHint != "" {
			// Support legacy /drive/root: and drive-specific /drives/{id}/root: paths.
			i := strings.Index(n.PathHint, "/root:")
			if i < 0 {
				return "", fmt.Errorf("unrecognized parent path %q", n.PathHint)
			}
			parent = strings.TrimPrefix(n.PathHint[i+len("/root:"):], "/")
			if strings.Contains(parent, "\\") || strings.Contains(parent, ":") || !filepath.IsLocal(filepath.FromSlash(path.Join(parent, n.Name))) {
				return "", fmt.Errorf("unsafe parent path %q", parent)
			}
		}
		rel := path.Join(parent, n.Name)
		if !filepath.IsLocal(filepath.FromSlash(rel)) {
			return "", fmt.Errorf("unsafe cloud path %q", rel)
		}
		p.paths[id] = rel
		return rel, nil
	}
	for _, it := range p.items {
		if it.Deleted != nil {
			old, err := store.GetByID(ctx, s.db, it.ID)
			if err == nil {
				p.paths[it.ID] = old.PathRel
				continue
			}
			if !errors.Is(err, sql.ErrNoRows) {
				return nil, err
			}
			if _, ok := nodes[it.ID]; !ok {
				continue
			}
		}
		if _, err := resolve(it.ID); err != nil {
			return nil, err
		}
	}
	if movedFolder {
		for id, n := range oldNodes {
			if !n.IsDir || n.IsRoot {
				continue
			}
			old, ok := storedNodePath(oldNodes, id, map[string]bool{})
			if !ok {
				continue
			}
			current, err := resolve(id)
			if err != nil {
				return nil, err
			}
			if old != "" && old != current {
				p.oldDirs = append(p.oldDirs, old)
				// Empty descendant directories are omitted from rename deltas too.
				if _, returned := last[id]; !returned && incremental {
					p.items = append(p.items, graph.DriveItem{ID: id, Name: n.Name, Folder: &graph.DriveItemFolder{}})
				}
			}
		}
	}
	// A folder rename does not enumerate descendants. Refresh known descendants
	// by ID, copying to the new paths before cleaning up their old locations.
	seen := map[string]bool{}
	for _, it := range p.items {
		seen[it.ID] = true
	}
	for id, n := range nodes {
		if !movedFolder || !incremental {
			break
		}
		if n.IsDir || n.IsRoot {
			continue
		}
		old, err := store.GetByID(ctx, s.db, id)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if seen[id] {
			if rel := p.paths[id]; rel != "" && rel != old.PathRel {
				p.oldPaths[id] = *old
			}
			continue
		}
		rel, err := resolve(id)
		if err != nil {
			return nil, err
		}
		if rel == old.PathRel {
			continue
		}
		it, err := s.g.GetItemByID(ctx, id)
		if err != nil {
			return nil, err
		}
		if it.Deleted != nil {
			p.deleted = append(p.deleted, id)
		} else {
			p.oldPaths[id] = *old
			p.nodes[id] = remoteNode(*it)
		}
		p.items = append(p.items, *it)
	}
	// Newly encountered file renames may not have a cached hierarchy node yet.
	for _, it := range p.items {
		if it.Deleted != nil || it.File == nil {
			continue
		}
		old, err := store.GetByID(ctx, s.db, it.ID)
		if err == nil && old.PathRel != p.paths[it.ID] {
			p.oldPaths[it.ID] = *old
		} else if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
	}
	if len(p.oldPaths) > 0 {
		pending, err := json.Marshal(p.oldPaths)
		if err != nil {
			return nil, err
		}
		if err := store.SetMeta(ctx, s.db, "pending_renames", string(pending)); err != nil {
			return nil, err
		}
	}
	return p, nil
}

func (s *Syncer) cleanupRenamedPaths(ctx context.Context, p *deltaPlan) error {
	occupied := map[string]bool{}
	for _, it := range p.items {
		if it.Deleted == nil {
			occupied[p.paths[it.ID]] = true
		}
	}
	var dirs []string
	for _, rel := range p.oldDirs {
		if !occupied[rel] {
			dirs = append(dirs, filepath.Join(s.cfg.LocalPath, filepath.FromSlash(rel)))
		}
	}
	for id, old := range p.oldPaths {
		if occupied[old.PathRel] {
			continue
		}
		// Do not remove excluded or conflict-only files that were not committed.
		current, err := store.GetByID(ctx, s.db, id)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return err
		}
		if current.PathRel != p.paths[id] {
			continue
		}
		lp := filepath.Join(s.cfg.LocalPath, filepath.FromSlash(old.PathRel))
		h, err := scan.HashFile(lp)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		if h != old.Shasum {
			newPath := filepath.Join(s.cfg.LocalPath, filepath.FromSlash(current.PathRel))
			if err := os.Rename(lp, makeConflictPath(newPath, "local")); err != nil {
				return err
			}
		} else if err := s.moveToTrash(lp); err != nil {
			return err
		}
		for dir := filepath.Dir(lp); ; dir = filepath.Dir(dir) {
			rel, err := filepath.Rel(s.cfg.LocalPath, dir)
			if err != nil || rel == "." || !filepath.IsLocal(rel) {
				break
			}
			dirs = append(dirs, dir)
		}
	}
	sort.Slice(dirs, func(i, j int) bool { return len(dirs[i]) > len(dirs[j]) })
	for _, dir := range dirs {
		children, err := os.ReadDir(dir)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		if len(children) == 0 {
			if err := os.Remove(dir); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
	}
	return nil
}

func storedNodePath(nodes map[string]store.RemoteNode, id string, visiting map[string]bool) (string, bool) {
	n, ok := nodes[id]
	if !ok || visiting[id] {
		return "", false
	}
	if n.IsRoot {
		return "", true
	}
	visiting[id] = true
	defer delete(visiting, id)
	parent := ""
	if n.ParentID != "" {
		var ok bool
		parent, ok = storedNodePath(nodes, n.ParentID, visiting)
		if !ok {
			return "", false
		}
	} else if n.PathHint != "" {
		i := strings.Index(n.PathHint, "/root:")
		if i < 0 {
			return "", false
		}
		parent = strings.TrimPrefix(n.PathHint[i+len("/root:"):], "/")
	}
	return path.Join(parent, n.Name), true
}
