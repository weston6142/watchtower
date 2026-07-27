package flow

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

type Lever string
type Gate string

const (
	LeverYolo    Lever = "yolo"
	LeverRegular Lever = "regular"
	LeverStrict  Lever = "strict"

	GateApproveArtifact Gate = "approve_artifact"
	GateDecisionQueue   Gate = "decision_queue"
	GateAuto            Gate = "auto"
)

type AgentRef struct {
	Package string `yaml:"package"`
	Model   string `yaml:"model"`
}

type Stage struct {
	Name       string     `yaml:"name"`
	Agents     []AgentRef `yaml:"agents"`
	Parallel   bool       `yaml:"parallel"`
	Completion string     `yaml:"completion"`
	Workspace  string     `yaml:"workspace"`
	Gate       Gate       `yaml:"gate"`
	Artifacts  []string   `yaml:"artifacts"`
	Retries    int        `yaml:"retries"`
	HeavySlot  bool       `yaml:"heavy_slot"`
}

type Flow struct {
	Name   string  `yaml:"name"`
	Stages []Stage `yaml:"stages"`
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
		if st.Completion == "" {
			st.Completion = "all"
		}
		if st.Completion != "all" && st.Completion != "any" {
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
		switch st.Gate {
		case GateApproveArtifact, GateDecisionQueue, GateAuto:
		default:
			return Flow{}, fmt.Errorf("stage %q bad gate %q", st.Name, st.Gate)
		}
	}
	return f, nil
}
