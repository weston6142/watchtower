package runtime

import (
	"github.com/weston6142/watchtower/internal/capability"
)

type ProcessRequest struct {
	Contract capability.CompiledContract
	Plan     capability.EnforcementPlan
	Path     string
	Args     []string
	Dir      string
	Env      []string
	Scratch  string
}

type Backend interface {
	Preflight(capability.CompiledContract) (capability.EnforcementPlan, error)
	Wrap(ProcessRequest) (ProcessRequest, error)
}
