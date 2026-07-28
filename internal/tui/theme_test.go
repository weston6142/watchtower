package tui

import "testing"

func TestThemeByName(t *testing.T) {
	if got := themeByName("tokyo-night").Accent; got != "#bb9af7" {
		t.Fatalf("tokyo-night accent = %q", got)
	}
	if got := themeByName("gruvbox").Heading; got != "#fabd2f" {
		t.Fatalf("gruvbox heading = %q", got)
	}
	if got := themeByName("terminal").Accent; got != "13" {
		t.Fatalf("terminal accent = %q", got)
	}
	// unknown name falls back to tokyo-night
	if got := themeByName("does-not-exist"); got != themeByName("tokyo-night") {
		t.Fatalf("fallback = %+v", got)
	}
}

func TestSetTheme(t *testing.T) {
	defer SetTheme("tokyo-night")
	SetTheme("catppuccin")
	if activeTheme.Accent != "#cba6f7" {
		t.Fatalf("activeTheme.Accent = %q", activeTheme.Accent)
	}
}
