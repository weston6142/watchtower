package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInvestigateCycleAgentResetsModel(t *testing.T) {
	s := investigateState{Field: 0, Model: 2}
	s = s.cycle(1)
	if s.agent() != "codex" || s.Model != 0 {
		t.Fatalf("agent=%s model=%d, want codex/0", s.agent(), s.Model)
	}
}

func TestBuildInvestigateCommandClaude(t *testing.T) {
	got := buildInvestigateCommand("claude", "opus-5", "low")
	want := "exec claude --model opus-5 --effort low --append-system-prompt " + shellQuote(investigationPrompt)
	if got != want {
		t.Fatalf("got %q", got)
	}
}

func TestBuildInvestigateCommandCodex(t *testing.T) {
	got := buildInvestigateCommand("codex", "gpt-5.6-luna", "xhigh")
	want := "exec codex -m gpt-5.6-luna -c model_reasoning_effort=xhigh " + shellQuote(investigationPrompt)
	if got != want {
		t.Fatalf("got %q", got)
	}
}

func TestShellQuote(t *testing.T) {
	if got := shellQuote("a'b"); got != `'a'\''b'` {
		t.Fatalf("got %q", got)
	}
}

func TestInvestigatePrefsRoundTrip(t *testing.T) {
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, ".watchtower"), 0o755); err != nil {
		t.Fatal(err)
	}
	saveInvestigatePrefs(repo, investigateState{Agent: 1, Model: 1, Effort: 3})
	s := loadInvestigatePrefs(repo)
	if s.Agent != 1 || s.Model != 1 || s.Effort != 3 {
		t.Fatalf("round trip = %+v", s)
	}
}

func TestLoadInvestigatePrefsMissing(t *testing.T) {
	s := loadInvestigatePrefs(t.TempDir())
	if s != (investigateState{}) {
		t.Fatalf("missing prefs should zero, got %+v", s)
	}
}

func TestEnsureWrapUpSkill(t *testing.T) {
	repo := t.TempDir()
	if err := ensureWrapUpSkill(repo); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(repo, ".claude", "skills", "watchtower-wrap-up", "SKILL.md")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "watchtower new") {
		t.Fatal("skill must reference watchtower new")
	}
	if err := ensureWrapUpSkill(repo); err != nil {
		t.Fatal(err)
	}
}

func TestRenderInvestigate(t *testing.T) {
	out := renderInvestigate(investigateState{}, 80)
	for _, want := range []string{"investigate", "AGENT", "MODEL", "EFFORT", "claude", "fable-5", "low"} {
		if !strings.Contains(out, want) {
			t.Fatalf("render missing %q:\n%s", want, out)
		}
	}
}
