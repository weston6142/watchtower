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
	"#61afef", "#c678dd", "#56b6c2", "#e78ac8", "#7d9bf0",
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
	seen := make(map[string]bool, len(order))
	for i, id := range order {
		tag := tagFor(id, titles[id])
		if seen[tag] {
			tag = digitsFor(id)
		}
		seen[tag] = true
		out[id] = Identity{Color: palette[i%len(palette)], Tag: tag}
	}
	return out
}

func digitsFor(issueID string) string {
	digits := strings.TrimPrefix(issueID, "GH-")
	if len([]rune(digits)) == 1 {
		digits = "0" + digits
	}
	return digits
}
