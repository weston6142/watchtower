package contextpack

import (
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/weston6142/watchtower/internal/decision"
	"github.com/weston6142/watchtower/internal/levers"
	"github.com/weston6142/watchtower/internal/stageresult"
)

type Artifact struct {
	Name   string `json:"name"`
	SHA256 string `json:"sha256"`
}

type Decision struct {
	Stage        string
	Question     string
	Kind         levers.DecisionKind
	Options      []string
	Response     levers.Response
	Why          string
	Consequences []string
	Status       string
	At           time.Time
	Context      *decision.DecisionContext
}

type Recovery struct {
	LastSuccessfulStage string
	CurrentHead         string
	Dirty               bool
	LastFailure         string
	OutstandingOutputs  []string
	ResultContext       *stageresult.RetryContext
}

type Brief struct {
	IssueID              string
	Stage                string
	StartCommit          string
	BaseCommit           string
	Branch               string
	RequiredInputs       []string
	ExpectedOutputs      []string
	AgentOwnedOutputs    []string
	EngineOwnedOutputs   []string
	ProhibitedActions    []string
	VerificationOwner    string
	FinalizationContract string
	Recovery             *Recovery
}

func Archive(sourceDir, issueDir string, names []string) ([]Artifact, error) {
	destination := filepath.Join(issueDir, "artifacts")
	if err := os.MkdirAll(destination, 0o755); err != nil {
		return nil, err
	}
	artifacts := make([]Artifact, 0, len(names))
	for _, name := range names {
		if err := validateName(name); err != nil {
			return nil, err
		}
		source := filepath.Join(sourceDir, filepath.FromSlash(name))
		info, err := os.Lstat(source)
		if err != nil {
			return nil, err
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("artifact %q is not a regular file", name)
		}
		digest, err := copyAtomic(source, filepath.Join(destination, filepath.FromSlash(name)))
		if err != nil {
			return nil, err
		}
		artifacts = append(artifacts, Artifact{Name: name, SHA256: digest})
	}
	return artifacts, nil
}

func Materialize(issueDir, workdir string, names []string) error {
	for _, name := range names {
		if err := validateName(name); err != nil {
			return err
		}
		source := filepath.Join(issueDir, "artifacts", filepath.FromSlash(name))
		info, err := os.Lstat(source)
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("durable artifact %q is not a regular file", name)
		}
		if _, err := copyAtomic(source, filepath.Join(workdir, filepath.FromSlash(name))); err != nil {
			return err
		}
	}
	return nil
}

func validateName(name string) error {
	if name == "" || filepath.IsAbs(name) {
		return fmt.Errorf("unsafe artifact name %q", name)
	}
	clean := filepath.Clean(filepath.FromSlash(name))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return fmt.Errorf("unsafe artifact name %q", name)
	}
	return nil
}

func copyAtomic(source, destination string) (string, error) {
	input, err := os.Open(source)
	if err != nil {
		return "", err
	}
	defer input.Close()
	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
		return "", err
	}
	temp, err := os.CreateTemp(filepath.Dir(destination), ".watchtower-artifact-*")
	if err != nil {
		return "", err
	}
	tempName := temp.Name()
	defer os.Remove(tempName)
	hash := sha256.New()
	if _, err := io.Copy(io.MultiWriter(temp, hash), input); err != nil {
		temp.Close()
		return "", err
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		return "", err
	}
	if err := temp.Close(); err != nil {
		return "", err
	}
	if err := os.Chmod(tempName, 0o644); err != nil {
		return "", err
	}
	if err := os.Rename(tempName, destination); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", hash.Sum(nil)), nil
}

func DecisionLedger(decisions []Decision) string {
	var body strings.Builder
	body.WriteString("# Accepted decisions\n")
	for _, decision := range decisions {
		if decision.Status != "answered" && decision.Status != "auto" {
			continue
		}
		answer := decision.Response.Text
		if decision.Response.Kind == levers.DecisionChoice &&
			decision.Response.Option != nil &&
			*decision.Response.Option >= 0 &&
			*decision.Response.Option < len(decision.Options) {
			answer = decision.Options[*decision.Response.Option]
		}
		body.WriteString("\n## " + decision.Question + "\n\n")
		body.WriteString("- Stage: " + decision.Stage + "\n")
		body.WriteString("- Time: " + decision.At.UTC().Format(time.RFC3339) + "\n")
		if decision.Context != nil {
			body.WriteString("- Task: " + decision.Context.TaskSummary + "\n")
			body.WriteString("- Agent: " + decision.Context.AgentLabel() + "\n")
		}
		body.WriteString("- Accepted response: " + answer + "\n")
		if decision.Why != "" {
			body.WriteString("- Rationale: " + decision.Why + "\n")
		}
		if len(decision.Consequences) > 0 {
			body.WriteString("- Consequences: " + strings.Join(decision.Consequences, "; ") + "\n")
		}
	}
	return body.String()
}

func WriteStageBrief(workdir string, brief Brief) error {
	var body strings.Builder
	body.WriteString("# Stage brief\n\n")
	body.WriteString("- Issue: " + brief.IssueID + "\n")
	body.WriteString("- Stage: " + brief.Stage + "\n")
	body.WriteString("- Start commit: " + valueOrUnknown(brief.StartCommit) + "\n")
	body.WriteString("- Base commit: " + valueOrUnknown(brief.BaseCommit) + "\n")
	body.WriteString("- Branch: " + valueOrUnknown(brief.Branch) + "\n")
	writeList(&body, "Required inputs", brief.RequiredInputs)
	agentOutputs := brief.AgentOwnedOutputs
	if len(agentOutputs) == 0 {
		agentOutputs = brief.ExpectedOutputs
	}
	writeList(&body, "Agent-owned outputs", agentOutputs)
	writeList(&body, "Engine-owned outputs", brief.EngineOwnedOutputs)
	writeList(&body, "Prohibited actions", brief.ProhibitedActions)
	body.WriteString("\n## Verification ownership\n\n")
	body.WriteString(valueOrUnknown(brief.VerificationOwner) + "\n")
	if brief.FinalizationContract != "" {
		body.WriteString("\n" + brief.FinalizationContract)
	}
	if brief.Recovery != nil {
		body.WriteString("\n## Recovery\n\n")
		body.WriteString("- Last successful stage: " + valueOrUnknown(brief.Recovery.LastSuccessfulStage) + "\n")
		body.WriteString("- Current HEAD: " + valueOrUnknown(brief.Recovery.CurrentHead) + "\n")
		dirty := "no"
		if brief.Recovery.Dirty {
			dirty = "yes"
		}
		body.WriteString("- dirty: " + dirty + "\n")
		body.WriteString("- Last failure: " + valueOrUnknown(brief.Recovery.LastFailure) + "\n")
		writeList(&body, "Outstanding outputs", brief.Recovery.OutstandingOutputs)
		writeStructuredRetryContext(&body, brief.Recovery.ResultContext)
	}
	return writeAtomic(filepath.Join(workdir, "STAGE.md"), []byte(body.String()))
}

func writeStructuredRetryContext(body *strings.Builder, context *stageresult.RetryContext) {
	if context == nil {
		return
	}
	body.WriteString("\n## Structured retry context\n\n")
	body.WriteString("- Source attempt: " + context.SourceAttemptID + "\n")
	unfinished := append(append([]stageresult.WorkItem(nil), context.UnfinishedPlanTasks...), context.RemainingWork...)
	writeWorkItems(body, "Unfinished work", unfinished)
	if len(context.OpenFindings) > 0 {
		body.WriteString("\n### Open findings\n\n")
		for _, finding := range context.OpenFindings {
			line := finding.ID + ": " + finding.Summary
			body.WriteString("- " + withPaths(line, finding.Paths) + "\n")
		}
	}
	if len(context.SkippedActivities) > 0 {
		body.WriteString("\n### Skipped activities\n\n")
		for _, skip := range context.SkippedActivities {
			body.WriteString("- " + skip.Activity + ": " + skip.Explanation + "\n")
		}
	}
	writeWorkItems(body, "Unreviewed paths", context.UnreviewedPaths)
	writeWorkItems(body, "Missing documentation", context.MissingDocumentation)
	if len(context.RemainingConcerns) > 0 {
		body.WriteString("\n### Remaining concerns\n\n")
		for _, concern := range context.RemainingConcerns {
			body.WriteString("- " + concern.Explanation + "\n")
		}
	}
}

func writeWorkItems(body *strings.Builder, title string, items []stageresult.WorkItem) {
	if len(items) == 0 {
		return
	}
	body.WriteString("\n### " + title + "\n\n")
	for _, item := range items {
		body.WriteString("- " + withPaths(item.Description, item.Paths) + "\n")
	}
}

func withPaths(text string, paths []string) string {
	if len(paths) == 0 {
		return text
	}
	return text + " (paths: " + strings.Join(paths, ", ") + ")"
}

func WriteDecisionLedger(workdir, ledger string) error {
	return writeAtomic(filepath.Join(workdir, "decisions.md"), []byte(ledger))
}

func writeList(body *strings.Builder, title string, values []string) {
	body.WriteString("\n## " + title + "\n\n")
	if len(values) == 0 {
		body.WriteString("- none\n")
		return
	}
	for _, value := range values {
		body.WriteString("- " + value + "\n")
	}
}

func valueOrUnknown(value string) string {
	if strings.TrimSpace(value) == "" {
		return "unknown"
	}
	return value
}

func writeAtomic(path string, content []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	temp, err := os.CreateTemp(filepath.Dir(path), ".watchtower-context-*")
	if err != nil {
		return err
	}
	name := temp.Name()
	defer os.Remove(name)
	if _, err := temp.Write(content); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(name, 0o644); err != nil {
		return err
	}
	return os.Rename(name, path)
}
