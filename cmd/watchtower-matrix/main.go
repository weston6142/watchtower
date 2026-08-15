package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/weston6142/watchtower/internal/engineharness"
	"github.com/weston6142/watchtower/internal/flow"
	"github.com/weston6142/watchtower/internal/recoverymatrix"
)

const evidenceVersion = 1

type matrixEvidence struct {
	Version          int                          `json:"version"`
	ManifestIdentity string                       `json:"manifest_identity"`
	Revision         string                       `json:"revision"`
	Runs             [2]recoverymatrix.RunSummary `json:"runs"`
	DeterminismPass  bool                         `json:"determinism_pass"`
}

func main() {
	if len(os.Args) < 2 {
		fatalf("usage: watchtower-matrix <run|complete> [flags]")
	}
	var err error
	switch os.Args[1] {
	case "run":
		err = runCommand(os.Args[2:])
	case "complete":
		err = completeCommand(os.Args[2:])
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		fatalf("%v", err)
	}
}

func runCommand(args []string) error {
	return runCommandTo(args, os.Stdout)
}

func runCommandTo(args []string, output io.Writer) error {
	flags := flag.NewFlagSet("run", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	manifestPath := flags.String("manifest", "", "validated recovery matrix manifest")
	flowPath := flags.String("flow", "", "production flow")
	revision := flags.String("revision", "", "production revision")
	evidencePath := flags.String("evidence", "", "full-run evidence path")
	scenarioID := flags.String("scenario", "", "diagnostic scenario ID")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *manifestPath == "" || *flowPath == "" || *revision == "" {
		return fmt.Errorf("--manifest, --flow, and --revision are required")
	}
	if *scenarioID != "" && *evidencePath != "" {
		return fmt.Errorf("--scenario cannot be combined with --evidence")
	}
	production, inventory, identity, err := loadInventory(*manifestPath, *flowPath)
	if err != nil {
		return err
	}
	factory := engineharness.NewFactory(production)
	if *scenarioID != "" {
		summary := recoverymatrix.Run(context.Background(), inventory, factory, recoverymatrix.RunOptions{
			ManifestIdentity: identity, Revision: *revision, ScenarioID: *scenarioID,
			ScenarioTimeout: 15 * 1000000000,
		})
		if err := writeJSON(output, summary); err != nil {
			return err
		}
		if summary.Executed != 1 || summary.Passed != 1 || summary.Failed != 0 || summary.Skipped != 0 ||
			summary.Panics != 0 || summary.Timeouts != 0 || summary.UnexpectedCalls != 0 ||
			summary.UnconsumedScripts != 0 || summary.MissingResults != 0 || len(summary.Results) != 1 ||
			summary.Results[0].Status != recoverymatrix.ResultPassed {
			return fmt.Errorf("diagnostic scenario %q did not execute exactly once and pass", *scenarioID)
		}
		return nil
	}
	first := recoverymatrix.Run(context.Background(), inventory, factory, recoverymatrix.RunOptions{
		ManifestIdentity: identity, Revision: *revision, ScenarioTimeout: 15 * 1000000000,
	})
	second := recoverymatrix.Run(context.Background(), inventory, factory, recoverymatrix.RunOptions{
		ManifestIdentity: identity, Revision: *revision, ScenarioTimeout: 15 * 1000000000,
	})
	if _, err := recoverymatrix.BuildCompletionReceipt(first, second, recoverymatrix.RepositoryVerification{Revision: *revision, Passed: true}); err != nil {
		return fmt.Errorf("deterministic matrix run failed: %w", err)
	}
	evidence := matrixEvidence{Version: evidenceVersion, ManifestIdentity: identity, Revision: *revision, Runs: [2]recoverymatrix.RunSummary{first, second}, DeterminismPass: true}
	if err := writeAtomicJSON(*evidencePath, evidence); err != nil {
		return err
	}
	return writeJSON(output, evidence)
}

func completeCommand(args []string) error {
	flags := flag.NewFlagSet("complete", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	evidencePath := flags.String("matrix-evidence", "", "matrix evidence")
	revision := flags.String("repository-revision", "", "repository revision")
	receiptPath := flags.String("receipt", "", "completion receipt")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *evidencePath == "" || *revision == "" || *receiptPath == "" {
		return fmt.Errorf("--matrix-evidence, --repository-revision, and --receipt are required")
	}
	var evidence matrixEvidence
	if err := readStrictJSON(*evidencePath, &evidence); err != nil {
		return fmt.Errorf("read matrix evidence: %w", err)
	}
	if evidence.Version != evidenceVersion || !evidence.DeterminismPass {
		return fmt.Errorf("matrix evidence is malformed or not deterministic")
	}
	if evidence.Revision != *revision {
		return fmt.Errorf("matrix evidence revision does not match repository revision")
	}
	receipt, err := recoverymatrix.BuildCompletionReceipt(evidence.Runs[0], evidence.Runs[1], recoverymatrix.RepositoryVerification{Revision: *revision, Passed: true})
	if err != nil {
		return err
	}
	if err := writeAtomicJSON(*receiptPath, receipt); err != nil {
		return err
	}
	return writeJSON(os.Stdout, receipt)
}

func loadInventory(manifestPath, flowPath string) (flow.Flow, []recoverymatrix.Scenario, string, error) {
	production, err := flow.Load(flowPath)
	if err != nil {
		return flow.Flow{}, nil, "", fmt.Errorf("load flow: %w", err)
	}
	manifest, err := recoverymatrix.LoadManifest(manifestPath)
	if err != nil {
		return flow.Flow{}, nil, "", fmt.Errorf("load manifest: %w", err)
	}
	validated, err := recoverymatrix.ValidateManifest(manifest, recoverymatrix.ResolveProduction(production))
	if err != nil {
		return flow.Flow{}, nil, "", fmt.Errorf("validate manifest: %w", err)
	}
	inventory, err := recoverymatrix.Compile(validated)
	if err != nil {
		return flow.Flow{}, nil, "", fmt.Errorf("compile manifest: %w", err)
	}
	identity, err := recoverymatrix.ManifestIdentity(validated)
	if err != nil {
		return flow.Flow{}, nil, "", fmt.Errorf("identify manifest: %w", err)
	}
	return production, inventory, identity, nil
}

func readStrictJSON(path string, target any) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	decoder := json.NewDecoder(file)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return fmt.Errorf("trailing JSON value")
		}
		return err
	}
	return nil
}

func writeAtomicJSON(path string, value any) error {
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(directory, ".watchtower-matrix-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return err
	}
	encoder := json.NewEncoder(temporary)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(value); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryPath, path)
}

func writeJSON(writer io.Writer, value any) error {
	return json.NewEncoder(writer).Encode(value)
}

func fatalf(format string, args ...any) {
	_, _ = fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
