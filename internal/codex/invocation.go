package codex

import (
	"regexp"
	"strconv"
	"strings"

	"github.com/weston6142/watchtower/internal/repocfg"
)

type turnKind string

const (
	turnInitial turnKind = "initial"
	turnResumed turnKind = "resumed"
)

type turnDescriptor struct {
	Workdir         string
	Kind            turnKind
	ResumeID        string
	PackagePrompt   string
	Prompt          string
	GatewayEndpoint string
	GatewayTools    []string
}

type invocation struct {
	Argv         []string
	RedactedArgv []string
}

// CodexProfileSetup is the safe, cached setup view for one Codex profile.
// Invocation tokens contain structural placeholders instead of turn content.
type CodexProfileSetup struct {
	Bin              string
	Model            string
	Effort           string
	FeatureOverrides map[string]bool
	InitialArgv      []string
	ResumedArgv      []string
}

func buildInvocation(profile repocfg.CodexProfile, descriptor turnDescriptor) invocation {
	var prefix []string
	if descriptor.Kind == turnResumed || descriptor.ResumeID != "" {
		prefix = []string{"exec", "resume", "--json"}
	} else {
		prefix = []string{"exec", "--json", "-C", descriptor.Workdir}
	}
	args := append([]string(nil), prefix...)
	args = append(args, "--strict-config", "--ignore-user-config", "--ignore-rules", "--skip-git-repo-check")
	args = append(args,
		"-m", profile.Model,
		"-c", configString("model_reasoning_effort", profile.Effort),
		"-c", configString("sandbox_mode", "read-only"),
		"-c", configString("approval_policy", "never"),
		"-c", "tools.web_search=false",
		"-c", "features.shell_tool=false",
		"-c", configString("developer_instructions", descriptor.PackagePrompt),
	)
	if descriptor.GatewayEndpoint != "" && len(descriptor.GatewayTools) > 0 {
		args = append(args,
			"-c", configString("mcp_servers.watchtower.url", descriptor.GatewayEndpoint),
			"-c", configStringList("mcp_servers.watchtower.enabled_tools", gatewayLocalToolNames(descriptor.GatewayTools)),
		)
	}
	if value, ok := profile.FeatureOverrides["unified_exec"]; ok {
		args = append(args, "-c", "features.unified_exec="+boolString(value))
	}
	if descriptor.Kind == turnResumed || descriptor.ResumeID != "" {
		args = append(args, descriptor.ResumeID)
	}
	args = append(args, descriptor.Prompt)

	return invocation{Argv: args, RedactedArgv: redactInvocation(args, descriptor)}
}

func DescribeCodexProfile(profile repocfg.CodexProfile) CodexProfileSetup {
	initial := buildInvocation(profile, turnDescriptor{
		Workdir:       "[worktree]",
		Kind:          turnInitial,
		PackagePrompt: "[developer instructions]",
		Prompt:        "[turn prompt]",
	})
	resumed := buildInvocation(profile, turnDescriptor{
		Kind:          turnResumed,
		ResumeID:      "[resume id]",
		PackagePrompt: "[developer instructions]",
		Prompt:        "[turn prompt]",
	})
	return CodexProfileSetup{
		Bin:              profile.Bin,
		Model:            profile.Model,
		Effort:           profile.Effort,
		FeatureOverrides: cloneCodexFeatures(profile.FeatureOverrides),
		InitialArgv:      initial.RedactedArgv,
		ResumedArgv:      resumed.RedactedArgv,
	}
}

func redactInvocation(args []string, descriptor turnDescriptor) []string {
	redacted := append([]string(nil), args...)
	for index := range redacted {
		switch {
		case redacted[index] == descriptor.Workdir && descriptor.Workdir != "":
			redacted[index] = "[redacted worktree]"
		case redacted[index] == descriptor.ResumeID && descriptor.ResumeID != "":
			redacted[index] = "[redacted resume id]"
		case redacted[index] == descriptor.Prompt && descriptor.Prompt != "":
			redacted[index] = "[redacted turn prompt]"
		case strings.HasPrefix(redacted[index], "developer_instructions="):
			redacted[index] = "developer_instructions=\"[redacted developer instructions]\""
		case strings.HasPrefix(redacted[index], "mcp_servers.watchtower.url="):
			redacted[index] = "mcp_servers.watchtower.url=\"[redacted gateway endpoint]\""
		}
	}
	return redacted
}

func gatewayLocalToolNames(tools []string) []string {
	result := make([]string, 0, len(tools))
	for _, tool := range tools {
		if name, ok := strings.CutPrefix(tool, "mcp__watchtower__"); ok && name != "" {
			result = append(result, name)
		}
	}
	return result
}

func configStringList(key string, values []string) string {
	quoted := make([]string, len(values))
	for index, value := range values {
		quoted[index] = strconv.Quote(value)
	}
	return key + "=[" + strings.Join(quoted, ",") + "]"
}

func redactText(value string, secrets ...string) string {
	for _, secret := range secrets {
		if secret != "" {
			value = strings.ReplaceAll(value, secret, "[redacted]")
		}
	}
	return sensitiveTextPattern.ReplaceAllString(value, "[redacted stderr]")
}

var sensitiveTextPattern = regexp.MustCompile(`(?i)\b[[:alnum:]_.-]*(secret|token|password|authorization)[[:alnum:]_.-]*\b`)

func boolString(value bool) string {
	if value {
		return "true"
	}
	return "false"
}

func cloneCodexFeatures(features map[string]bool) map[string]bool {
	if features == nil {
		return nil
	}
	clone := make(map[string]bool, len(features))
	for name, value := range features {
		clone[name] = value
	}
	return clone
}
