package tui

import (
	"fmt"
	"strings"
	"testing"
)

func TestPagerScrollClamps(t *testing.T) {
	p := pagerState{Mode: "pager", Lines: mklines(100)}
	p = p.scroll("G", 20)
	if p.Top != 80 {
		t.Fatalf("G: %d", p.Top)
	}
	p = p.scroll("j", 20)
	if p.Top != 80 {
		t.Fatalf("clamp: %d", p.Top)
	}
	p = p.scroll("g", 20)
	if p.Top != 0 {
		t.Fatalf("g: %d", p.Top)
	}
	p = p.scroll("d", 20)
	if p.Top != 10 {
		t.Fatalf("d: %d", p.Top)
	}
}

func TestRenderPagerWindow(t *testing.T) {
	p := pagerState{Mode: "pager", Title: "spec.md", Lines: mklines(100), Top: 50}
	out := renderPager(p, 80, 10)
	if !strings.Contains(out, "line-50") || strings.Contains(out, "line-70") {
		t.Fatalf("window wrong:\n%s", out)
	}
}

func TestArtifactListWearsBoxChrome(t *testing.T) {
	p := pagerState{Mode: "artifacts", Files: []string{"a.md", "b.md"}, Sel: 1, Title: "GH-1"}
	out := renderArtifactList(p, Identity{Tag: "◆"}, 80, 20)
	if !strings.Contains(out, "─") {
		t.Fatal("artifact list has no border")
	}
	if !strings.Contains(out, glyphCursor) {
		t.Fatal("selected row has no cursor glyph")
	}
	if !strings.Contains(out, "esc back") {
		t.Fatal("missing esc hint")
	}
}

func mklines(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("line-%d", i)
	}
	return out
}
