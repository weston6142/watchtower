package tui

import "testing"

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
	if ids["GH-1"].Color != "#e06c75" {
		t.Fatalf("palette order broken: %s", ids["GH-1"].Color)
	}
}
