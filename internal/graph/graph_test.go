package graph

import "testing"

func TestEscapePathSegments(t *testing.T) {
	got := escapePathSegments("dir/子/space file.txt")
	want := "/dir/%E5%AD%90/space%20file.txt"
	if got != want {
		t.Fatalf("escapePathSegments got %s want %s", got, want)
	}
}
