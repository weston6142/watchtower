package runtime_test

import (
	"context"
	"testing"

	"github.com/weston6142/watchtower/internal/capability"
	capruntime "github.com/weston6142/watchtower/internal/capability/runtime"
)

type incompleteBackend struct{}

func (incompleteBackend) Preflight(capability.CompiledContract) (capability.EnforcementPlan, error) {
	return capability.EnforcementPlan{}, &capability.PolicyError{Phase: "preflight", Reason: capability.ReasonProviderUnsupported, Diagnostic: "containment control missing"}
}

func (incompleteBackend) Wrap(capruntime.ProcessRequest) (capruntime.ProcessRequest, error) {
	return capruntime.ProcessRequest{}, nil
}

func TestContainmentPreflightFailsClosed(t *testing.T) {
	_, err := capruntime.Start(context.Background(), capruntime.StartRequest{Backend: incompleteBackend{}})
	if err == nil {
		t.Fatal("incomplete containment backend started a session")
	}
}
