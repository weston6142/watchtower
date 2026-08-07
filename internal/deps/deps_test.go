package deps

import (
	"errors"
	"reflect"
	"testing"
)

func TestActiveBlockersPreservesOrderAndFailsClosed(t *testing.T) {
	resolved := map[string]bool{"done": true, "cleanup": true}
	got, err := ActiveBlockers([]string{"active", "done", "unknown", "cleanup"},
		func(id string) (bool, error) { return resolved[id], nil })
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"active", "unknown"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("active blockers = %v, want %v", got, want)
	}
	_, err = ActiveBlockers([]string{"active"}, func(string) (bool, error) {
		return false, errors.New("status lookup failed")
	})
	if err == nil || err.Error() != "status lookup failed" {
		t.Fatalf("lookup error = %v", err)
	}
}

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
