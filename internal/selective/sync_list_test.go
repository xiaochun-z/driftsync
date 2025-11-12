package selective

import (
	"os"
	"testing"
)

func TestShouldSyncBasic(t *testing.T) {
	l, err := Load("")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !l.ShouldSync("a/b.txt", false) {
		t.Fatalf("default should sync")
	}
}

func TestIncludeExclude(t *testing.T) {
	tmp := t.TempDir()
	path := tmp + "/sync_list"
	content := "/docs\n-*.tmp\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	l, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}

	if !l.ShouldSync("docs/readme.md", false) {
		t.Fatal("should include /docs")
	}
	if l.ShouldSync("misc/a.tmp", false) {
		t.Fatal("should exclude *.tmp")
	}
}

func TestTextList_IncludeDocs_ExcludeWorkJson(t *testing.T) {
	// 文本清单规则：
	//  1) 包含 /docs 整个子树
	//  2) 排除 /docs/work.json 单个文件（排除优先级高于包含）
	tmp := t.TempDir()
	path := tmp + "/sync_list"
	content := "/docs\n-/docs/work.json\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	l, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	// 命中包含
	if !l.ShouldSync("docs/readme.md", false) {
		t.Fatalf("docs/readme.md should be included")
	}
	if !l.ShouldSync("docs/sub/note.txt", false) {
		t.Fatalf("docs/sub/note.txt should be included")
	}

	// 被精确排除
	if l.ShouldSync("docs/work.json", false) {
		t.Fatalf("docs/work.json should be excluded")
	}

	// 相邻但不同名的文件不受排除影响
	if !l.ShouldSync("docs/work2.json", false) {
		t.Fatalf("docs/work2.json should be included")
	}

	// 子目录下的 work.json 也不该被这条单文件排除命中
	if !l.ShouldSync("docs/sub/work.json", false) {
		t.Fatalf("docs/sub/work.json should be included (exclude only targets /docs/work.json)")
	}
}

func TestYAML_IncludeDocs_ExcludeWorkJson(t *testing.T) {
	// include: ["/docs"], exclude: ["/docs/work.json"]
	l := FromYAML([]string{"/docs"}, []string{"/docs/work.json"})

	if !l.HasRules {
		t.Fatalf("yaml rules should be loaded")
	}

	if !l.ShouldSync("docs/readme.md", false) {
		t.Fatalf("docs/readme.md should be included")
	}
	if !l.ShouldSync("docs/sub/note.txt", false) {
		t.Fatalf("docs/sub/note.txt should be included")
	}
	if l.ShouldSync("docs/work.json", false) {
		t.Fatalf("docs/work.json should be excluded")
	}
	if !l.ShouldSync("docs/work2.json", false) {
		t.Fatalf("docs/work2.json should be included")
	}
	if !l.ShouldSync("docs/sub/work.json", false) {
		t.Fatalf("docs/sub/work.json should be included (exclude only targets /docs/work.json)")
	}
}
