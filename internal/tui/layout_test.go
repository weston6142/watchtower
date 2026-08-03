package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

var gh11FooterTerms = []string{
	"j/k", "tab", "pause/resume", "kill", "retry", "stream", "levers", "help", "quit",
}

func gh11Lines(t *testing.T, m Model) []string {
	t.Helper()
	plain := strings.TrimRight(ansi.Strip(m.View()), "\n")
	if plain == "" {
		t.Fatal("tower view is empty")
	}
	return strings.Split(plain, "\n")
}

func gh11Footer(t *testing.T, lines []string) (int, string) {
	t.Helper()
	for i, line := range lines {
		if strings.Contains(line, "j/k") {
			return i, strings.Join(lines[i:], "\n")
		}
	}
	t.Fatalf("footer keybar is missing from view:\n%s", strings.Join(lines, "\n"))
	return 0, ""
}

func requireGH11View(t *testing.T, m Model, width, height int) []string {
	t.Helper()
	lines := gh11Lines(t, m)
	if len(lines) != height {
		t.Fatalf("view at %dx%d rendered %d rows, want exactly %d", width, height, len(lines), height)
	}
	for i, line := range lines {
		if got := lipgloss.Width(line); got > width {
			t.Fatalf("view at %dx%d row %d is %d cells wide:\n%s", width, height, i, got, strings.Join(lines, "\n"))
		}
	}
	footerStart, footer := gh11Footer(t, lines)
	for _, term := range gh11FooterTerms {
		if !strings.Contains(footer, term) {
			t.Fatalf("view at %dx%d footer lost %q:\n%s", width, height, term, footer)
		}
	}
	if strings.Count(footer, "j/k") != 1 || strings.Count(footer, "quit") != 1 {
		t.Fatalf("view at %dx%d duplicated footer items:\n%s", width, height, footer)
	}
	if strings.Contains(strings.Join(lines[:2], "\n"), "floors") {
		t.Fatalf("header still owns the floor shortcut:\n%s", strings.Join(lines[:2], "\n"))
	}
	if footerStart <= 2 {
		t.Fatalf("footer overlaps the header/main order at row %d", footerStart)
	}
	return lines
}

func requireGH11Surface(t *testing.T, flow string, width, height int) []string {
	t.Helper()
	return requireGH11View(t, FixtureModel(flow, width, height), width, height)
}

func TestGH11TowerViewDocksFooterAndUsesMainHeight(t *testing.T) {
	for _, flow := range []string{"floor", "rows"} {
		t.Run(flow, func(t *testing.T) {
			lines := requireGH11Surface(t, flow, 100, 40)
			if !strings.Contains(lines[0], "1 question for you") {
				t.Fatalf("header moved or disappeared: %q", lines[0])
			}
			if strings.TrimSpace(lines[2]) != "" {
				t.Fatalf("main content is pinned directly under the header: %q", lines[2])
			}
			mainNeedle := "MAP ·"
			if flow == "rows" {
				mainNeedle = "ROWS · z tower"
			}
			mainStart := -1
			for i, line := range lines {
				if strings.Contains(line, mainNeedle) {
					mainStart = i
					break
				}
			}
			if mainStart <= 2 {
				t.Fatalf("main region has no breathing space: start row %d", mainStart)
			}
		})
	}
}

func TestGH11TowerViewFitsResponsiveMatrix(t *testing.T) {
	for _, tc := range []struct {
		name          string
		width, height int
	}{
		{name: "wide", width: 200, height: 50},
		{name: "normal", width: 100, height: 40},
		{name: "short", width: 100, height: 24},
		{name: "narrow", width: 60, height: 40},
	} {
		for _, flow := range []string{"floor", "rows"} {
			t.Run(flow+"-"+tc.name, func(t *testing.T) {
				lines := requireGH11Surface(t, flow, tc.width, tc.height)
				plain := strings.Join(lines, "\n")
				terms := []string{"dark-mode audit", "SHIPPED today", "PARKED", "DECISION QUEUE"}
				if flow == "floor" {
					terms = append(terms, "CA", "GH")
				} else {
					terms = append(terms, "create a repo", "issue importer")
				}
				for _, want := range terms {
					if !strings.Contains(plain, want) {
						t.Fatalf("%s at %dx%d lost existing content %q:\n%s", flow, tc.width, tc.height, want, plain)
					}
				}
			})
		}
	}
}

func TestGH11ResizePreservesStateAndShortcutEffects(t *testing.T) {
	initial := FixtureModel("floor", 200, 50)
	wantAfterResize := ansi.Strip(initial.View())
	wantAfterNavigation := ansi.Strip(pressKey(t, initial, "j").View())
	m := initial
	for _, size := range [][2]int{{200, 50}, {100, 24}, {60, 40}, {200, 50}} {
		next, _ := m.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
		m = next.(Model)
		requireGH11View(t, m, size[0], size[1])
	}
	if got := ansi.Strip(m.View()); got != wantAfterResize {
		t.Fatalf("resize round-trip changed the rendered state:\n got:\n%s\nwant:\n%s", got, wantAfterResize)
	}
	if got := ansi.Strip(pressKey(t, m, "j").View()); got != wantAfterNavigation {
		t.Fatalf("navigation after resize changed its rendered effect:\n got:\n%s\nwant:\n%s", got, wantAfterNavigation)
	}
}

func TestGH11ViewIsSafeBeforePositiveWindowSize(t *testing.T) {
	m := FixtureModel("floor", 0, 0)
	plain := ansi.Strip(m.View())
	if !strings.Contains(plain, "1 question for you") || !strings.Contains(plain, "j/k") {
		t.Fatalf("zero-size view lost readable chrome:\n%s", plain)
	}
}
