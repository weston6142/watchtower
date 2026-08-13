package engine

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/weston6142/watchtower/internal/runner"
)

func TestAgentNeverReceivesLifecycleAuthority(t *testing.T) {
	e, _, _ := verificationEngine(t, "merge", [][]string{{"true"}}, "")
	fake := e.cfg.Runner.(*runner.FakeRunner)
	fake.OnEnvironment = func(_, _, _, _ string, env []string) {
		if len(env) != 0 {
			t.Fatalf("provider environment contains lifecycle values: %v", env)
		}
	}
	fake.OnStart = func(_, _, _, workdir string) error {
		if _, err := os.Lstat(filepath.Join(workdir, "verification.json")); !os.IsNotExist(err) {
			return &testLifecycleError{"verification receipt exists before provider exits"}
		}
		return nil
	}

	requestType := reflect.TypeOf(runner.StageRequest{})
	for _, forbidden := range []string{"Lease", "Receipt", "Rebase", "Merge", "Push", "Cleanup", "Daemon", "Publication", "Credential"} {
		for index := 0; index < requestType.NumField(); index++ {
			if strings.Contains(strings.ToLower(requestType.Field(index).Name), strings.ToLower(forbidden)) {
				t.Fatalf("runner request exposes lifecycle field %s", requestType.Field(index).Name)
			}
		}
	}
	id, err := e.CreateIssue("lifecycle isolation", "", "default", nil, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.StartIssue(context.Background(), id); err != nil {
		t.Fatal(err)
	}
}

func TestLifecycleRefusesMissingOrFailedPolicyEvidence(t *testing.T) {
	e, _, _ := verificationEngine(t, "merge", [][]string{{"true"}}, "")
	fake := e.cfg.Runner.(*runner.FakeRunner)
	script := fake.Scripts["merge-verification/merge-verifier"]
	script.Artifacts["verification.json"] = `{"passed":true}`
	fake.Scripts["merge-verification/merge-verifier"] = script
	id, err := e.CreateIssue("agent receipt", "", "default", nil, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.StartIssue(context.Background(), id); err == nil || !strings.Contains(err.Error(), "capability") {
		t.Fatalf("agent-authored lifecycle evidence error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(e.issueDir(id), "artifacts", "verification.json")); !os.IsNotExist(err) {
		t.Fatalf("rejected agent receipt became durable engine evidence: %v", err)
	}
}

type testLifecycleError struct{ message string }

func (e *testLifecycleError) Error() string { return e.message }
