
package selective

import "testing"

func TestShouldSyncBasic(t *testing.T) {
	l, err := Load("")
	if err != nil { t.Fatalf("load: %v", err) }
	if !l.ShouldSync("a/b.txt", false) { t.Fatalf("default should sync") }
}

func TestIncludeExclude(t *testing.T) {
	// Simulate a list in-memory: include /docs, exclude *.tmp
	tmp := t.TempDir()
	path := tmp + "/sync_list"
	content := "/docs
-*.tmp
"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil { t.Fatal(err) }
	l, err := Load(path)
	if err != nil { t.Fatal(err) }

	if !l.ShouldSync("docs/readme.md", false) { t.Fatal("should include /docs") }
	if l.ShouldSync("misc/a.tmp", false) { t.Fatal("should exclude *.tmp") }
}
