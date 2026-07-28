package tui

import "github.com/charmbracelet/lipgloss"

// Theme is the six-token palette driving overlays and the toast. Text runs
// never set a background color; Panel is only the foreground of the inverted
// close chip so it reads against the Accent background.
type Theme struct {
	Accent  lipgloss.Color // keys, modal border, close chip background
	Heading lipgloss.Color // section labels, footer key-labels
	Text    lipgloss.Color // descriptions, body text
	Dim     lipgloss.Color // subtitles, secondary text
	Bright  lipgloss.Color // modal title
	Panel   lipgloss.Color // chip text on accent background
}

var themes = map[string]Theme{
	"tokyo-night": {Accent: "#bb9af7", Heading: "#7aa2f7", Text: "#c0caf5", Dim: "#565f89", Bright: "#e4ecff", Panel: "#1f2335"},
	"terminal":    {Accent: "13", Heading: "12", Text: "7", Dim: "8", Bright: "15", Panel: "0"},
	"catppuccin":  {Accent: "#cba6f7", Heading: "#89b4fa", Text: "#cdd6f4", Dim: "#6c7086", Bright: "#f0f4ff", Panel: "#181825"},
	"gruvbox":     {Accent: "#d3869b", Heading: "#fabd2f", Text: "#ebdbb2", Dim: "#928374", Bright: "#fbf1c7", Panel: "#32302f"},
}

func themeByName(name string) Theme {
	if t, ok := themes[name]; ok {
		return t
	}
	return themes["tokyo-night"]
}

var activeTheme = themeByName("tokyo-night")

// SetTheme selects the active preset by name; unknown names keep the
// tokyo-night default.
func SetTheme(name string) { activeTheme = themeByName(name) }
