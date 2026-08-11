package runtime

import (
	"github.com/weston6142/watchtower/internal/capability"
)

type ProcessMode string

const (
	ModeAgentOperation    ProcessMode = "agent-operation"
	ModeProviderTransport ProcessMode = "provider-transport"
)

type ProcessRequest struct {
	Contract            capability.CompiledContract
	Plan                capability.EnforcementPlan
	Mode                ProcessMode
	Path                string
	Args                []string
	Dir                 string
	Env                 []string
	Scratch             string
	ProviderReads       []string
	ProviderExecutables []string
}

type Backend interface {
	Preflight(capability.CompiledContract) (capability.EnforcementPlan, error)
	Wrap(ProcessRequest) (ProcessRequest, error)
}
