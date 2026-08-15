package engineharness

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRepositoryGateOrderAndSmokeSeparation(t *testing.T) {
	contents, err := os.ReadFile(filepath.Join("..", "..", "scripts", "verify"))
	if err != nil {
		t.Fatal(err)
	}
	script := string(contents)
	markers := []string{"watchtower-matrix run", "go test ./... -race", "go vet ./...", "go build ./...", "git diff --check", "watchtower-matrix complete"}
	previous := -1
	for _, marker := range markers {
		index := strings.Index(script, marker)
		if index < 0 || index <= previous {
			t.Fatalf("gate marker %q is out of order", marker)
		}
		previous = index
	}
	if strings.Contains(strings.ToLower(script), "provider smoke") {
		t.Fatal("real-provider smoke is in the normal gate")
	}
}
