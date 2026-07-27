package levers

import (
	"path"
	"strings"

	"github.com/wbushyeager/guildhall/internal/flow"
)

type Matrix map[string]flow.Lever

func Preset(f flow.Flow, l flow.Lever) Matrix {
	m := Matrix{}
	for _, st := range f.Stages {
		m[st.Name] = l
	}
	return m
}

type Decision struct {
	Question    string
	Options     []string
	Recommended int
	Importance  float64
	Paths       []string
}

type Rules struct {
	AlwaysEscalate []string
}

func matches(pattern, p string) bool {
	if strings.HasSuffix(pattern, "/**") {
		return strings.HasPrefix(p, strings.TrimSuffix(pattern, "**"))
	}
	ok, _ := path.Match(pattern, p)
	return ok
}

func Route(d Decision, lever flow.Lever, rules Rules) bool {
	if d.Importance >= 1.0 {
		return true // built-in floor
	}
	for _, pat := range rules.AlwaysEscalate {
		for _, p := range d.Paths {
			if matches(pat, p) {
				return true
			}
		}
	}
	switch lever {
	case flow.LeverStrict:
		return true
	case flow.LeverRegular:
		return d.Importance >= 0.5
	default: // yolo
		return d.Importance >= 0.9
	}
}
