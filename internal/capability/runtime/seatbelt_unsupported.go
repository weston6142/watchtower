//go:build !darwin

package runtime

import "github.com/weston6142/watchtower/internal/capability"

type unsupportedBackend struct{}

func NewPlatformBackend() Backend { return unsupportedBackend{} }

func (unsupportedBackend) Preflight(capability.CompiledContract) (capability.EnforcementPlan, error) {
	return capability.EnforcementPlan{}, unsupported("platform containment is unsupported")
}

func (unsupportedBackend) Wrap(ProcessRequest) (ProcessRequest, error) {
	return ProcessRequest{}, unsupported("platform containment is unsupported")
}
