
package syncer

import "testing"

func TestConflictName(t *testing.T) {
	n := conflictName("notes/today.md", "local-conflict")
	if n == "notes/today.md" || n == "" {
		t.Fatalf("conflict name should differ and not be empty")
	}
}
