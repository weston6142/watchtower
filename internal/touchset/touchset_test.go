package touchset

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestCanonicalScopeRejectsUnsafePaths(t *testing.T) {
	for _, value := range []string{"", ".", "*", "**", "**/*", "/tmp/x", `docs\\x`, "../x", "docs/../x", ".git/config", "x/.watchtower/state", "x\x00y"} {
		if _, err := CanonicalGlob(value); err == nil {
			t.Errorf("CanonicalGlob(%q) succeeded", value)
		}
	}
	if got, err := CanonicalPath("./docs/guide.md"); err != nil || got != "docs/guide.md" {
		t.Fatalf("CanonicalPath = %q, %v", got, err)
	}
	if got, err := CanonicalGlobs([]string{"docs/**", "./docs/**", "go.mod"}); err != nil ||
		!reflect.DeepEqual(got, []string{"docs/**", "go.mod"}) {
		t.Fatalf("CanonicalGlobs = %v, %v", got, err)
	}
}

func TestMatchUsesCanonicalTouchsetSemantics(t *testing.T) {
	tests := []struct {
		glob string
		path string
		want bool
	}{
		{"docs/**", "docs/guide/deep.md", true},
		{"docs/**", "docs", false},
		{"doc/**", "docs/guide.md", false},
		{"docs-draft-*", "docs-draft-api", true},
		{"internal/*/test.go", "internal/flow/test.go", true},
		{"internal/*/test.go", "internal/a/b/test.go", false},
		{"go.mod", "go.mod", true},
	}
	for _, test := range tests {
		got, err := Match(test.glob, test.path)
		if err != nil || got != test.want {
			t.Errorf("Match(%q, %q) = %v, %v, want %v", test.glob, test.path, got, err, test.want)
		}
	}
}

func TestLoadAndOverlap(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "touchset.json")
	if err := os.WriteFile(p, []byte(`{"globs":["internal/pay/**","go.mod"]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	a, err := Load(p)
	if err != nil || len(a.Globs) != 2 {
		t.Fatalf("load: %+v %v", a, err)
	}
	cases := []struct {
		b    []string
		want bool
	}{
		{[]string{"internal/pay/refund.go"}, true},
		{[]string{"internal/payments/**"}, false},
		{[]string{"docs/**"}, false},
		{[]string{"go.mod"}, true},
		{[]string{"internal/**"}, true},
	}
	for _, c := range cases {
		if got := Overlap(a, Set{Globs: c.b}); got != c.want {
			t.Errorf("overlap(%v)=%v want %v", c.b, got, c.want)
		}
	}
}
