package tui

import "github.com/charmbracelet/lipgloss"

// Theme is the 13-token palette. The color contract: Accent means "enter
// does something here"; Warn means "a human is needed"; Ok/Err are outcomes
// only; Structure identifies (nouns, groups, working agents) and never
// signals need. Hues never trade jobs.
type Theme struct {
	Bg0       lipgloss.Color // canvas — content ground
	Bg1       lipgloss.Color // chrome — header/footer bars, cards
	Bg2       lipgloss.Color // selection — cursor rows, card header bands
	Bg3       lipgloss.Color // key chips
	Accent    lipgloss.Color // focus & agency
	Structure lipgloss.Color // nouns & identity (help groups, arch modules, working)
	Ok        lipgloss.Color // outcomes: done, shipped, additions
	Warn      lipgloss.Color // attention: need-you, questions, ★
	Err       lipgloss.Color // outcomes: failed, deletions, errors
	Text      lipgloss.Color // body
	Dim       lipgloss.Color // secondary
	Dimmer    lipgloss.Color // idle glyphs, ghost content
	Bright    lipgloss.Color // titles
}

const defaultThemeName = "tokyo-night"

var themes = map[string]Theme{
	defaultThemeName: {
		Bg0: "#16161e", Bg1: "#1a1b26", Bg2: "#1f2335", Bg3: "#292e42",
		Accent: "#bb9af7", Structure: "#7aa2f7",
		Ok: "#9ece6a", Warn: "#e0af68", Err: "#f7768e",
		Text: "#c0caf5", Dim: "#565f89", Dimmer: "#3b4261", Bright: "#e4ecff",
	},
	"terminal": {
		// ANSI-16 approximation: two grounds only; glyphs carry state.
		Bg0: "0", Bg1: "0", Bg2: "8", Bg3: "8",
		Accent: "13", Structure: "12",
		Ok: "10", Warn: "11", Err: "9",
		Text: "7", Dim: "8", Dimmer: "8", Bright: "15",
	},
	"catppuccin": {
		Bg0: "#181825", Bg1: "#1e1e2e", Bg2: "#313244", Bg3: "#45475a",
		Accent: "#cba6f7", Structure: "#89b4fa",
		Ok: "#a6e3a1", Warn: "#f9e2af", Err: "#f38ba8",
		Text: "#cdd6f4", Dim: "#6c7086", Dimmer: "#45475a", Bright: "#f0f4ff",
	},
	"gruvbox": {
		Bg0: "#282828", Bg1: "#32302f", Bg2: "#3c3836", Bg3: "#504945",
		Accent: "#d3869b", Structure: "#83a598",
		Ok: "#b8bb26", Warn: "#fabd2f", Err: "#fb4934",
		Text: "#ebdbb2", Dim: "#928374", Dimmer: "#665c54", Bright: "#fbf1c7",
	},
}

func themeByName(name string) Theme {
	if t, ok := themes[name]; ok {
		return t
	}
	return themes[defaultThemeName]
}

var activeTheme = themeByName(defaultThemeName)

// SetTheme selects the active preset by name; unknown names keep the default.
func SetTheme(name string) { activeTheme = themeByName(name) }
