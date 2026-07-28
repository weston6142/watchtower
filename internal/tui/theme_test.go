package tui

import "testing"

func TestThemePresetsComplete(t *testing.T) {
	for name, th := range themes {
		for field, v := range map[string]string{
			"Bg0": string(th.Bg0), "Bg1": string(th.Bg1), "Bg2": string(th.Bg2), "Bg3": string(th.Bg3),
			"Accent": string(th.Accent), "Structure": string(th.Structure),
			"Ok": string(th.Ok), "Warn": string(th.Warn), "Err": string(th.Err),
			"Text": string(th.Text), "Dim": string(th.Dim), "Dimmer": string(th.Dimmer), "Bright": string(th.Bright),
		} {
			if v == "" {
				t.Errorf("theme %q: token %s is empty", name, field)
			}
		}
	}
}

func TestThemeByNameFallsBack(t *testing.T) {
	if themeByName("nope") != themes[defaultThemeName] {
		t.Error("unknown theme should fall back to default")
	}
}

func TestSetThemeSelectsPreset(t *testing.T) {
	defer SetTheme(defaultThemeName)
	SetTheme("gruvbox")
	if activeTheme != themes["gruvbox"] {
		t.Error("SetTheme should activate the named preset")
	}
}
