package tui

import (
	"flag"
	"os"
	"path/filepath"
	"testing"
)

var update = flag.Bool("update", false, "rewrite golden snapshot files")

func TestSnapshots(t *testing.T) {
	for _, flow := range FixtureFlows() {
		for _, size := range []struct {
			name          string
			width, height int
		}{{"wide", 200, 50}, {"narrow", 100, 40}} {
			t.Run(flow+"-"+size.name, func(t *testing.T) {
				got := SnapshotFlow(flow, size.width, size.height)
				golden := filepath.Join("testdata", flow+"-"+size.name+".golden")
				if *update {
					if err := os.MkdirAll("testdata", 0o755); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(golden, []byte(got), 0o644); err != nil {
						t.Fatal(err)
					}
					return
				}
				want, err := os.ReadFile(golden)
				if err != nil {
					t.Fatalf("missing golden %s — run: go test ./internal/tui -run TestSnapshots -update", golden)
				}
				if string(want) != got {
					t.Errorf("%s render drifted from golden; if intentional run with -update", flow)
				}
			})
		}
	}
}
