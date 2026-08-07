package runner

import (
	"context"
	"strings"
	"testing"
)

func TestManagedEnvironmentMergeReplacesManagedEntries(t *testing.T) {
	merged := MergeEnvironment(
		[]string{"GOCACHE=inherited-cache", "GOMODCACHE=inherited-mod", "SENTINEL=keep"},
		[]string{"GOCACHE=extra-cache", "GOPATH=extra-path", "EXTRA=preserve"},
		[]string{"GOCACHE=lease-cache", "GOMODCACHE=lease-mod", "GOPATH=lease-path", "GOCACHE=lease-cache"},
	)
	values := make(map[string][]string)
	for _, entry := range merged {
		key, value, ok := strings.Cut(entry, "=")
		if ok {
			values[key] = append(values[key], value)
		}
	}
	for key, want := range map[string]string{
		"GOCACHE":    "lease-cache",
		"GOMODCACHE": "lease-mod",
		"GOPATH":     "lease-path",
		"SENTINEL":   "keep",
		"EXTRA":      "preserve",
	} {
		if got := values[key]; len(got) != 1 || got[0] != want {
			t.Fatalf("%s = %v, want one %q", key, got, want)
		}
	}
}

func TestManagedEnvironmentContextCopiesOverlay(t *testing.T) {
	original := []string{"GOCACHE=lease-cache"}
	ctx := WithManagedEnvironment(context.Background(), original)
	original[0] = "GOCACHE=mutated"
	got := ManagedEnvironment(ctx)
	if len(got) != 1 || got[0] != "GOCACHE=lease-cache" {
		t.Fatalf("context overlay = %v, want copied lease value", got)
	}
	got[0] = "GOCACHE=changed"
	if again := ManagedEnvironment(ctx); again[0] != "GOCACHE=lease-cache" {
		t.Fatalf("context overlay was mutable through accessor: %v", again)
	}
}
