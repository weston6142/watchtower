//go:build darwin

package runtime

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/weston6142/watchtower/internal/capability"
	"github.com/weston6142/watchtower/internal/runner"
	"github.com/weston6142/watchtower/internal/touchset"
)

type seatbeltBackend struct {
	executable string
}

func NewPlatformBackend() Backend {
	path, _ := exec.LookPath("sandbox-exec")
	return &seatbeltBackend{executable: path}
}

func (b *seatbeltBackend) Preflight(contract capability.CompiledContract) (capability.EnforcementPlan, error) {
	if b == nil || b.executable == "" {
		return capability.EnforcementPlan{}, unsupported("Darwin Seatbelt is unavailable")
	}
	controls := runner.RequiredControls(contract)
	proofs := make([]capability.ControlProof, 0, len(controls))
	for _, control := range controls {
		proofs = append(proofs, capability.ControlProof{Control: control, Proven: true})
	}
	return runner.NewEnforcementPlan(contract, "capability-runtime", "darwin-seatbelt", "1", proofs)
}

func (b *seatbeltBackend) Wrap(request ProcessRequest) (ProcessRequest, error) {
	if b == nil || b.executable == "" {
		return ProcessRequest{}, unsupported("Darwin Seatbelt is unavailable")
	}
	if err := runner.ValidateEnforcementPlan(request.Contract, request.Plan); err != nil {
		return ProcessRequest{}, err
	}
	profile, err := seatbeltProfile(request)
	if err != nil {
		return ProcessRequest{}, err
	}
	return ProcessRequest{
		Contract: request.Contract, Plan: request.Plan, Path: b.executable,
		Args: append([]string{"-p", profile, "--", request.Path}, request.Args...),
		Dir:  request.Dir, Env: append([]string(nil), request.Env...), Scratch: request.Scratch,
	}, nil
}

func seatbeltProfile(request ProcessRequest) (string, error) {
	if request.Path == "" || !filepath.IsAbs(request.Path) {
		return "", unsupported("contained executable path is invalid")
	}
	reads := []string{request.Path, "/System", "/usr/lib", "/usr/bin", "/bin", "/dev/null", "/private/etc"}
	if request.Scratch != "" {
		reads = append(reads, request.Scratch)
	}
	for _, path := range request.Contract.Contract.Reads {
		reads = append(reads, filepath.Join(request.Contract.Contract.WorkspaceRoot, filepath.FromSlash(path)))
	}
	writes := []string{}
	if request.Scratch != "" {
		writes = append(writes, request.Scratch)
	}
	for _, grant := range request.Contract.Contract.Writes {
		prefix := touchset.PrefixOf(grant.Path)
		if prefix == "" {
			return "", unsupported("write grant has no safe containment prefix")
		}
		writes = append(writes, filepath.Join(request.Contract.Contract.WorkspaceRoot, filepath.FromSlash(prefix)))
	}
	sort.Strings(reads)
	sort.Strings(writes)
	var profile strings.Builder
	profile.WriteString("(version 1)\n(deny default)\n(import \"system.sb\")\n(deny network*)\n")
	profile.WriteString("(allow process-exec (literal \"")
	profile.WriteString(seatbeltEscape(request.Path))
	profile.WriteString("\"))\n(allow process-fork process-info*)\n(allow signal (target self))\n")
	profile.WriteString("(allow syscall*)\n(allow sysctl-read)\n(allow mach*)\n(allow ipc*)\n(allow system*)\n")
	profile.WriteString("(allow file-read-metadata file-test-existence file-map-executable file-ioctl)\n")
	for _, path := range reads {
		operator := "literal"
		if info, err := filepath.Glob(path); err == nil && len(info) > 0 {
			if stat, statErr := filepath.EvalSymlinks(path); statErr == nil && stat != path {
				path = stat
			}
		}
		if filepath.Ext(path) == "" || path == "/System" || path == "/usr/lib" || path == "/usr/bin" || path == "/bin" || path == "/private/etc" {
			operator = "subpath"
		}
		fmt.Fprintf(&profile, "(allow file-read* (%s \"%s\"))\n", operator, seatbeltEscape(path))
	}
	for _, path := range writes {
		fmt.Fprintf(&profile, "(allow file-write* (subpath \"%s\"))\n", seatbeltEscape(path))
	}
	return profile.String(), nil
}

func seatbeltEscape(value string) string {
	value = strings.ReplaceAll(value, `\`, `\\`)
	return strings.ReplaceAll(value, `"`, `\"`)
}
