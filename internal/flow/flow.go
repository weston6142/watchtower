package flow

import (
	"fmt"
	"os"
	"strings"

	"github.com/weston6142/watchtower/internal/touchset"
	"gopkg.in/yaml.v3"
)

type Lever string
type Gate string
type CapabilityProfile string

const (
	LeverYolo    Lever = "yolo"
	LeverRegular Lever = "regular"
	LeverStrict  Lever = "strict"

	GateApproveArtifact Gate = "approve_artifact"
	GateDecisionQueue   Gate = "decision_queue"
	GateAuto            Gate = "auto"
	GatePlanReview      Gate = "plan_review"

	// CompletionAll requires every agent in the stage to succeed;
	// CompletionAny requires at least one.
	CompletionAll = "all"
	CompletionAny = "any"

	ProfileArtifact           CapabilityProfile = "artifact"
	ProfileInspect            CapabilityProfile = "inspect"
	ProfileImplementation     CapabilityProfile = "implementation"
	ProfileReview             CapabilityProfile = "review"
	ProfileLibrarian          CapabilityProfile = "librarian"
	ProfileFinalReview        CapabilityProfile = "final-review"
	ProfileConflictResolution CapabilityProfile = "conflict-resolution"
)

type AgentRef struct {
	Package string `yaml:"package"`
	Model   string `yaml:"model"`
}

type Stage struct {
	Name               string            `yaml:"name"`
	Agents             []AgentRef        `yaml:"agents"`
	Parallel           bool              `yaml:"parallel"`
	Completion         string            `yaml:"completion"`
	Workspace          string            `yaml:"workspace"`
	Gate               Gate              `yaml:"gate"`
	Artifacts          []string          `yaml:"artifacts"`
	CapabilityProfile  CapabilityProfile `yaml:"capability_profile"`
	DocumentationPaths []string          `yaml:"documentation_paths"`
	Retries            int               `yaml:"retries"`
	HeavySlot          bool              `yaml:"heavy_slot"`
	MergeBarrier       bool              `yaml:"merge_barrier"`
}

type Flow struct {
	Name   string  `yaml:"name"`
	Stages []Stage `yaml:"stages"`
}

func (f Flow) StageNames() []string {
	names := make([]string, len(f.Stages))
	for i := range f.Stages {
		names[i] = f.Stages[i].Name
	}
	return names
}

var FinalizationArtifacts = []string{
	"merge-report.md",
	"merge-decision.json",
	"verification.json",
}

func (s Stage) DeclaresArtifact(name string) bool {
	for _, artifact := range s.Artifacts {
		if artifact == name {
			return true
		}
	}
	return false
}

func (f Flow) IntegrationStage() (Stage, int, bool) {
	for index, stage := range f.Stages {
		if stage.MergeBarrier {
			return stage, index, true
		}
	}
	return Stage{}, 0, false
}

func (f Flow) ValidateIntegration(testArgv []string) error {
	if _, _, ok := f.IntegrationStage(); ok && len(testArgv) == 0 {
		return fmt.Errorf("flow %q requires test_cmd because it has a merge barrier", f.Name)
	}
	return nil
}

func Load(path string) (Flow, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Flow{}, err
	}
	return loadBytes(b)
}

func loadBytes(b []byte) (Flow, error) {
	var f Flow
	if err := yaml.Unmarshal(b, &f); err != nil {
		return Flow{}, err
	}
	if len(f.Stages) == 0 {
		return Flow{}, fmt.Errorf("flow %q has no stages", f.Name)
	}
	seen := map[string]bool{}
	for i := range f.Stages {
		st := &f.Stages[i]
		if seen[st.Name] {
			return Flow{}, fmt.Errorf("duplicate stage %q", st.Name)
		}
		seen[st.Name] = true
		if len(st.Agents) == 0 {
			return Flow{}, fmt.Errorf("stage %q has no agents", st.Name)
		}
		switch st.CapabilityProfile {
		case ProfileArtifact, ProfileInspect, ProfileImplementation, ProfileReview,
			ProfileLibrarian, ProfileFinalReview, ProfileConflictResolution:
		default:
			return Flow{}, fmt.Errorf("stage %q capability_profile %q is missing or unknown", st.Name, st.CapabilityProfile)
		}
		if st.CapabilityProfile == ProfileLibrarian {
			if len(st.DocumentationPaths) == 0 {
				return Flow{}, fmt.Errorf("stage %q librarian capability_profile requires documentation_paths", st.Name)
			}
			canonical, err := touchset.CanonicalGlobs(st.DocumentationPaths)
			if err != nil {
				return Flow{}, fmt.Errorf("stage %q documentation_paths: %w", st.Name, err)
			}
			st.DocumentationPaths = canonical
		} else if len(st.DocumentationPaths) != 0 {
			return Flow{}, fmt.Errorf("stage %q documentation_paths are valid only for librarian capability_profile", st.Name)
		}
		if st.Completion == "" {
			st.Completion = CompletionAll
		}
		if st.Completion != CompletionAll && st.Completion != CompletionAny {
			return Flow{}, fmt.Errorf("stage %q bad completion %q", st.Name, st.Completion)
		}
		if st.Workspace == "" {
			st.Workspace = "none"
		}
		switch st.Workspace {
		case "none", "worktree", "readonly":
		default:
			return Flow{}, fmt.Errorf("stage %q bad workspace %q", st.Name, st.Workspace)
		}
		if st.Workspace == "readonly" && len(st.Artifacts) > 0 {
			return Flow{}, fmt.Errorf("stage %q readonly workspace contradicts writable artifacts", st.Name)
		}
		switch st.Gate {
		case GateApproveArtifact, GateDecisionQueue, GateAuto, GatePlanReview:
		default:
			return Flow{}, fmt.Errorf("stage %q bad gate %q", st.Name, st.Gate)
		}
	}
	barrierIndex := -1
	for index, stage := range f.Stages {
		if !stage.MergeBarrier {
			continue
		}
		if barrierIndex >= 0 {
			return Flow{}, fmt.Errorf("flow %q has more than one merge barrier", f.Name)
		}
		barrierIndex = index
	}
	if barrierIndex >= 0 {
		stage := f.Stages[barrierIndex]
		if barrierIndex != len(f.Stages)-1 {
			return Flow{}, fmt.Errorf(
				"flow %q merge barrier %q must be the final stage", f.Name, stage.Name)
		}
		var missing []string
		for _, artifact := range FinalizationArtifacts {
			if !stage.DeclaresArtifact(artifact) {
				missing = append(missing, artifact)
			}
		}
		if len(missing) > 0 {
			return Flow{}, fmt.Errorf(
				"flow %q merge barrier %q missing artifacts: %s",
				f.Name, stage.Name, strings.Join(missing, ", "))
		}
	}
	return f, nil
}
