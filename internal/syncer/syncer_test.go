package syncer

import "testing"

func TestConflictName(t *testing.T) {
	n := makeConflictName("notes/today.md", "local-conflict")
	if n == "notes/today.md" ||
		n == "" {
		t.Fatalf("conflict name should differ and not be empty")
	}
}
