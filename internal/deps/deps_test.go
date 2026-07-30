package deps

import "testing"

func TestGraphValidatesAcyclicDependencies(t *testing.T) {
	graph := Graph{
		"GH-1": nil,
		"GH-2": {"GH-1"},
		"GH-3": {"GH-1", "GH-2"},
	}
	if err := graph.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestGraphRejectsInvalidDependencies(t *testing.T) {
	cases := map[string]Graph{
		"self":      {"GH-1": {"GH-1"}},
		"duplicate": {"GH-1": nil, "GH-2": {"GH-1", "GH-1"}},
		"unknown":   {"GH-1": {"GH-404"}},
		"cycle":     {"GH-1": {"GH-2"}, "GH-2": {"GH-1"}},
	}
	for name, graph := range cases {
		t.Run(name, func(t *testing.T) {
			if err := graph.Validate(); err == nil {
				t.Fatal("invalid graph was accepted")
			}
		})
	}
}
