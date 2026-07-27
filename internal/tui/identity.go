package tui

import (
	"strings"
)

// Identity is an issue's stable visual identity.
type Identity struct {
	Color string
	Tag   string
}

var palette = []string{
	"#e06c75", "#61afef", "#98c379", "#e5c07b",
	"#c678dd", "#56b6c2", "#d19a66", "#abb2bf",
}

func tagFor(issueID, title string) string {
	words := strings.Fields(strings.ToUpper(title))
	switch {
	case len(words) >= 2:
		return string([]rune(words[0])[0]) + string([]rune(words[1])[0])
	case len(words) == 1:
		runes := []rune(words[0])
		if len(runes) >= 2 {
			return string(runes[:2])
		}
	}
	digits := strings.TrimPrefix(issueID, "GH-")
	if len([]rune(digits)) == 1 {
		digits = "0" + digits
	}
	return digits
}

// Identify assigns colors by creation order and derives a short title tag.
func Identify(order []string, titles map[string]string) map[string]Identity {
	out := make(map[string]Identity, len(order))
	for i, id := range order {
		out[id] = Identity{Color: palette[i%len(palette)], Tag: tagFor(id, titles[id])}
	}
	return out
}
