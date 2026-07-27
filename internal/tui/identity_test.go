package tui

import "testing"

func TestPaletteExcludesStatusHues(t *testing.T) {
	banned := map[string]bool{"#e06c75": true, "#f2c14e": true, "#98c379": true, "#e5c07b": true}
	for _, c := range palette {
		if banned[c] {
			t.Fatalf("status hue %s in identity palette", c)
		}
	}
	if len(palette) != 5 {
		t.Fatalf("palette size %d", len(palette))
	}
}

func TestIdentifyDedupesTags(t *testing.T) {
	order := []string{"GH-1", "GH-2"}
	titles := map[string]string{"GH-1": "fix auth", "GH-2": "fix api"}
	ids := Identify(order, titles)
	if ids["GH-1"].Tag == ids["GH-2"].Tag {
		t.Fatalf("tags collide: %+v", ids)
	}
	if ids["GH-2"].Tag != "02" {
		t.Fatalf("fallback wrong: %q", ids["GH-2"].Tag)
	}
}

func TestIdentify(t *testing.T) {
	order := []string{"GH-1", "GH-2", "GH-3"}
	titles := map[string]string{"GH-1": "payment adapter", "GH-2": "Fixnpe", "GH-3": ""}
	ids := Identify(order, titles)
	if ids["GH-1"].Tag != "PA" || ids["GH-2"].Tag != "FI" || ids["GH-3"].Tag != "03" {
		t.Fatalf("tags: %+v", ids)
	}
	if ids["GH-1"].Color == ids["GH-2"].Color {
		t.Fatal("adjacent issues share a color")
	}
	if ids["GH-1"].Color != "#61afef" {
		t.Fatalf("palette order broken: %s", ids["GH-1"].Color)
	}
}
