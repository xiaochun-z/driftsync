package store

import (
	"context"
	"path/filepath"
	"testing"
)

func TestUpsertFailurePreservesPreviousPathOwner(t *testing.T) {
	ctx := context.Background()
	db, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := UpsertItem(ctx, db, Item{ID: "old", PathRel: "a.txt"}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TRIGGER reject_new BEFORE INSERT ON items WHEN new.id='new' BEGIN SELECT RAISE(ABORT,'simulated write failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := UpsertItem(ctx, db, Item{ID: "new", PathRel: "a.txt"}); err == nil {
		t.Fatal("expected write failure")
	}
	it, err := GetByPathFull(ctx, db, "a.txt")
	if err != nil || it.ID != "old" {
		t.Fatalf("previous owner lost: %v %v", it, err)
	}
}

func TestCheckpointFailureRollsBackHierarchyAndRetainsRecoveryState(t *testing.T) {
	ctx := context.Background()
	db, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := SetMeta(ctx, db, "pending_renames", "recovery state"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TRIGGER reject_cursor BEFORE INSERT ON meta WHEN new.key='delta_link' BEGIN SELECT RAISE(ABORT,'simulated cursor failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := CommitCheckpoint(ctx, db, "new", map[string]RemoteNode{"f": {ID: "f", Name: "a.txt"}}, nil, false); err == nil {
		t.Fatal("expected checkpoint failure")
	}
	nodes, err := ListRemoteNodes(ctx, db)
	if err != nil || len(nodes) != 0 {
		t.Fatalf("partial hierarchy committed: %v %v", nodes, err)
	}
	if pending, err := GetMeta(ctx, db, "pending_renames"); err != nil || pending != "recovery state" {
		t.Fatalf("recovery state lost: %q %v", pending, err)
	}
}
