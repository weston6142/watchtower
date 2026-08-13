package runtime_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/weston6142/watchtower/internal/capability"
	"github.com/weston6142/watchtower/internal/flow"
)

func TestSessionGatewayListsAndExecutesOnlyContractOperations(t *testing.T) {
	workdir := t.TempDir()
	if err := os.WriteFile(filepath.Join(workdir, "ISSUE.md"), []byte("issue\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	contract := compileRuntimeContract(t, capability.CompileInput{
		IssueID: "GH-68", Stage: "spec", AttemptID: "gateway-1", Profile: flow.ProfileArtifact,
		WorkspaceRoot: workdir, MaterializedInputs: []string{"ISSUE.md"},
		Outputs: []capability.RequiredOutput{{Path: "spec.md", Owner: capability.OwnerAgent}},
	})
	session := startRuntimeSession(t, workdir, contract)
	defer session.Close()

	listed := gatewayRequest(t, session.GatewayEndpoint(), map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/list",
	})
	encoded, err := json.Marshal(listed["result"])
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"workspace_read", "workspace_mutate"} {
		if !bytes.Contains(encoded, []byte(`"name":"`+want+`"`)) {
			t.Fatalf("gateway tools omitted %s: %s", want, encoded)
		}
	}
	for _, forbidden := range []string{"push", "merge", "rebase", "publication", "verification"} {
		if bytes.Contains(bytes.ToLower(encoded), []byte(forbidden)) {
			t.Fatalf("gateway exposed lifecycle authority %q: %s", forbidden, encoded)
		}
	}

	written := gatewayRequest(t, session.GatewayEndpoint(), map[string]any{
		"jsonrpc": "2.0", "id": 2, "method": "tools/call",
		"params": map[string]any{"name": "workspace_mutate", "arguments": map[string]any{
			"action": "write", "path": "spec.md", "content": "mediated\n", "mutation": "create",
		}},
	})
	if written["error"] != nil || toolResultIsError(written) {
		t.Fatalf("mediated write failed: %#v", written)
	}
	if body, err := os.ReadFile(filepath.Join(workdir, "spec.md")); err != nil || string(body) != "mediated\n" {
		t.Fatalf("mediated write body=%q err=%v", body, err)
	}

	denied := gatewayRequest(t, session.GatewayEndpoint(), map[string]any{
		"jsonrpc": "2.0", "id": 3, "method": "tools/call",
		"params": map[string]any{"name": "workspace_read", "arguments": map[string]any{"path": "secret.txt"}},
	})
	if !toolResultIsError(denied) {
		t.Fatalf("undeclared gateway read was accepted: %#v", denied)
	}
}

func TestSessionGatewayRejectsUnknownAndMalformedCalls(t *testing.T) {
	workdir := t.TempDir()
	contract := compileRuntimeContract(t, capability.CompileInput{
		IssueID: "GH-68", Stage: "inspect", AttemptID: "gateway-2", Profile: flow.ProfileInspect,
		WorkspaceRoot: workdir,
	})
	session := startRuntimeSession(t, workdir, contract)
	defer session.Close()

	for _, request := range []map[string]any{
		{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": "workspace_destroy", "arguments": map[string]any{}}},
		{"jsonrpc": "2.0", "id": 2, "method": "tools/call", "params": map[string]any{"name": "workspace_read", "arguments": map[string]any{"unexpected": true}}},
	} {
		response := gatewayRequest(t, session.GatewayEndpoint(), request)
		if !toolResultIsError(response) {
			t.Fatalf("gateway accepted malformed request: %#v", response)
		}
	}
}

func gatewayRequest(t *testing.T, endpoint string, request map[string]any) map[string]any {
	t.Helper()
	body, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	httpRequest, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	httpRequest.Header.Set("Content-Type", "application/json")
	httpRequest.Header.Set("Accept", "application/json, text/event-stream")
	response, err := http.DefaultClient.Do(httpRequest)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	encoded, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("gateway status=%d body=%s", response.StatusCode, encoded)
	}
	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("decode gateway response %q: %v", encoded, err)
	}
	return decoded
}

func toolResultIsError(response map[string]any) bool {
	result, _ := response["result"].(map[string]any)
	isError, _ := result["isError"].(bool)
	return isError
}
