package pkgs

import "testing"

func TestLoadDir(t *testing.T) {
	m, err := LoadDir("testdata")
	if err != nil {
		t.Fatal(err)
	}
	p, ok := m["executor"]
	if !ok {
		t.Fatalf("executor missing: %v", m)
	}
	if len(p.AllowedTools) != 4 || p.AllowedTools[0] != "Bash" {
		t.Fatalf("tools: %v", p.AllowedTools)
	}
	if p.Prompt == "" || p.Name != "executor" {
		t.Fatalf("bad package: %+v", p)
	}
}
