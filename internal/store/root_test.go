package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestBindSyncRootResetsOnlySyncHistory(t *testing.T) {
	ctx := context.Background()
	db, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	oldRoot, newRoot := t.TempDir(), t.TempDir()
	if _, err := BindSyncRoot(ctx, db, oldRoot); err != nil {
		t.Fatal(err)
	}
	if err := UpsertItem(ctx, db, Item{ID: "id", PathRel: "old.txt"}); err != nil {
		t.Fatal(err)
	}
	if err := SetMeta(ctx, db, "delta_link", "old-cursor"); err != nil {
		t.Fatal(err)
	}
	tokens := NewTokenStore(db)
	if err := tokens.Save(ctx, &Tokens{AccessToken: "access", RefreshToken: "refresh", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if reset, err := BindSyncRoot(ctx, db, filepath.Join(oldRoot, ".")); err != nil || reset {
		t.Fatalf("same root reset: %v %v", reset, err)
	}
	if reset, err := BindSyncRoot(ctx, db, newRoot); err != nil || !reset {
		t.Fatalf("new root: %v %v", reset, err)
	}
	paths, err := ListAllPaths(ctx, db)
	if err != nil || len(paths) != 0 {
		t.Fatalf("retained old history: %v %v", paths, err)
	}
	if link, _ := GetMeta(ctx, db, "delta_link"); link != "" {
		t.Fatalf("retained cursor: %q", link)
	}
	if tok, err := tokens.Load(ctx); err != nil || tok.RefreshToken != "refresh" {
		t.Fatalf("lost credentials: %v %v", tok, err)
	}
}

func TestBindLegacyRootDropsUnboundDeletionHistory(t *testing.T) {
	ctx := context.Background()
	db, err := Open(filepath.Join(t.TempDir(), "legacy.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := UpsertItem(ctx, db, Item{ID: "old", PathRel: "old.txt"}); err != nil {
		t.Fatal(err)
	}
	if _, err := BindSyncRoot(ctx, db, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	paths, err := ListAllPaths(ctx, db)
	if err != nil || len(paths) != 0 {
		t.Fatalf("unsafe legacy history: %v %v", paths, err)
	}
}
