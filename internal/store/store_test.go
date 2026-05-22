package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestUpsertAndGetByPath(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	it := Item{
		ID:       "id1",
		PathRel:  "docs/readme.md",
		ETag:     "etag1",
		Size:     1024,
		Mtime:    1000,
		Shasum:   "abc123",
		LastSrc:  "local",
		LastSync: time.Now().Unix(),
	}
	if err := UpsertItem(ctx, db, it); err != nil {
		t.Fatalf("UpsertItem: %v", err)
	}

	got, err := GetByPathFull(ctx, db, "docs/readme.md")
	if err != nil {
		t.Fatalf("GetByPathFull: %v", err)
	}
	if got.ID != "id1" {
		t.Fatalf("ID: got %q", got.ID)
	}
	if got.Shasum != "abc123" {
		t.Fatalf("Shasum: got %q", got.Shasum)
	}
	if got.LastSrc != "local" {
		t.Fatalf("LastSrc: got %q", got.LastSrc)
	}
}

func TestUpsertItem_Update(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	it := Item{ID: "id1", PathRel: "a.txt", ETag: "e1", Size: 10, Shasum: "old"}
	if err := UpsertItem(ctx, db, it); err != nil {
		t.Fatalf("first upsert: %v", err)
	}

	it.Shasum = "new"
	it.ETag = "e2"
	if err := UpsertItem(ctx, db, it); err != nil {
		t.Fatalf("second upsert: %v", err)
	}

	got, _ := GetByPathFull(ctx, db, "a.txt")
	if got.Shasum != "new" {
		t.Fatalf("expected updated shasum, got %q", got.Shasum)
	}
	if got.ETag != "e2" {
		t.Fatalf("expected updated etag, got %q", got.ETag)
	}
}

func TestGetByID(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	UpsertItem(ctx, db, Item{ID: "xyz", PathRel: "b.txt", ETag: "e", Shasum: "s"})

	got, err := GetByID(ctx, db, "xyz")
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.PathRel != "b.txt" {
		t.Fatalf("PathRel: got %q", got.PathRel)
	}
}

func TestGetByID_NotFound(t *testing.T) {
	db := openTestDB(t)
	_, err := GetByID(context.Background(), db, "missing")
	if err == nil {
		t.Fatal("expected error for missing ID")
	}
}

func TestDeleteByPath(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	UpsertItem(ctx, db, Item{ID: "d1", PathRel: "del.txt"})

	if err := DeleteByPath(ctx, db, "del.txt"); err != nil {
		t.Fatalf("DeleteByPath: %v", err)
	}

	_, err := GetByPathFull(ctx, db, "del.txt")
	if err == nil {
		t.Fatal("expected error after deletion")
	}
}

func TestListAllPaths(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	UpsertItem(ctx, db, Item{ID: "l1", PathRel: "a.txt"})
	UpsertItem(ctx, db, Item{ID: "l2", PathRel: "b.txt"})

	paths, err := ListAllPaths(ctx, db)
	if err != nil {
		t.Fatalf("ListAllPaths: %v", err)
	}
	if len(paths) != 2 {
		t.Fatalf("expected 2 paths, got %d", len(paths))
	}
}

func TestUpsertItem_PathConflictDifferentID(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	UpsertItem(ctx, db, Item{ID: "old-id", PathRel: "shared.txt", Shasum: "old"})
	if err := UpsertItem(ctx, db, Item{ID: "new-id", PathRel: "shared.txt", Shasum: "new"}); err != nil {
		t.Fatalf("upsert with conflicting path: %v", err)
	}

	got, _ := GetByPathFull(ctx, db, "shared.txt")
	if got.ID != "new-id" {
		t.Fatalf("expected new-id after path conflict resolution, got %q", got.ID)
	}
}

func TestSetAndGetMeta(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	if err := SetMeta(ctx, db, "delta_link", "https://example.com/delta"); err != nil {
		t.Fatalf("SetMeta: %v", err)
	}

	val, err := GetMeta(ctx, db, "delta_link")
	if err != nil {
		t.Fatalf("GetMeta: %v", err)
	}
	if val != "https://example.com/delta" {
		t.Fatalf("meta value: got %q", val)
	}
}

func TestSetMeta_Overwrite(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	SetMeta(ctx, db, "key", "v1")
	SetMeta(ctx, db, "key", "v2")

	val, _ := GetMeta(ctx, db, "key")
	if val != "v2" {
		t.Fatalf("expected v2 after overwrite, got %q", val)
	}
}

func TestGetMeta_Missing(t *testing.T) {
	db := openTestDB(t)
	_, err := GetMeta(context.Background(), db, "nonexistent")
	if err == nil {
		t.Fatal("expected error for missing meta key")
	}
}

func TestTokenStore_SaveAndLoad(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	ts := NewTokenStore(db)
	tok := &Tokens{
		AccessToken:  "access",
		RefreshToken: "refresh",
		ExpiresAt:    time.Unix(9999999, 0),
	}
	if err := ts.Save(ctx, tok); err != nil {
		t.Fatalf("Save: %v", err)
	}

	loaded, err := ts.Load(ctx)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.AccessToken != "access" {
		t.Fatalf("AccessToken: got %q", loaded.AccessToken)
	}
	if loaded.RefreshToken != "refresh" {
		t.Fatalf("RefreshToken: got %q", loaded.RefreshToken)
	}
	if loaded.ExpiresAt.Unix() != 9999999 {
		t.Fatalf("ExpiresAt: got %v", loaded.ExpiresAt)
	}
}

func TestTokenStore_Overwrite(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	ts := NewTokenStore(db)
	ts.Save(ctx, &Tokens{AccessToken: "old", RefreshToken: "r", ExpiresAt: time.Now()})
	ts.Save(ctx, &Tokens{AccessToken: "new", RefreshToken: "r2", ExpiresAt: time.Now()})

	loaded, _ := ts.Load(ctx)
	if loaded.AccessToken != "new" {
		t.Fatalf("expected 'new' after overwrite, got %q", loaded.AccessToken)
	}
}
