package runner

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/weston6142/watchtower/internal/levers"
)

func TestFakeRunnerAsksThenProduces(t *testing.T) {
	dir := t.TempDir()
	var got levers.Response
	fr := &FakeRunner{Scripts: map[string]Script{
		"spec/spec-writer": {
			Asks:      []levers.Decision{{Question: "REST or GraphQL?", Options: []string{"REST", "GraphQL"}, Recommended: 0, Importance: 0.6}},
			Artifacts: map[string]string{"spec.md": ""},
			Tokens:    42,
		},
	}, OnResponse: func(_ string, _ string, response levers.Response) {
		got = response
	}}
	asks := make(chan Ask, 1)
	done := fr.Run(context.Background(), "GH-1", "spec", "spec-writer", dir, asks)

	a := <-asks
	if a.Decision.Question != "REST or GraphQL?" {
		t.Fatalf("wrong ask: %+v", a.Decision)
	}
	a.Reply <- levers.ChoiceResponse(0)

	res := <-done
	if res.Err != nil || res.Tokens != 42 {
		t.Fatalf("bad result: %+v", res)
	}
	p := res.Artifacts["spec.md"]
	if p != filepath.Join(dir, "spec.md") {
		t.Fatalf("artifact path wrong: %q", p)
	}
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("artifact not written: %v", err)
	}
	if got.Kind != levers.DecisionChoice || got.Option == nil || *got.Option != 0 {
		t.Fatalf("response = %#v", got)
	}
}
