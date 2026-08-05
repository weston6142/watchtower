package plannerartifact

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const plannerSessionEnv = "WATCHTOWER_PLANNER_SESSION"

type Session struct {
	workdir       string
	stateDir      string
	removeState   bool
	manifest      *Manifest
	completedKeys []string
}

func Initialize(workdir string) (*Session, error) {
	if err := validateWorkdir(workdir); err != nil {
		return nil, err
	}
	plan, touchset, fresh, err := inspectTargets(workdir)
	if err != nil {
		return nil, err
	}
	if fresh {
		if err := createFreshTargets(workdir); err != nil {
			return nil, err
		}
		plan = []byte("# Implementation Plan\n\n")
		touchset = []byte(`{"globs":[]}`)
	}
	document, err := parsePlan(plan)
	if err != nil {
		return nil, malformedStartingError("plan.md", err)
	}
	set, err := parseTouchset(touchset)
	if err != nil {
		return nil, malformedStartingError("touchset.json", err)
	}
	if len(document.Sections) == 0 && len(set.Globs) != 0 {
		return nil, malformedStartingError("pair", fmt.Errorf("unanchored plan has touchset globs"))
	}
	stateDir, err := os.MkdirTemp("", "watchtower-planner-session-")
	if err != nil {
		return nil, fmt.Errorf("create planner session: %w", err)
	}
	if err := os.Chmod(stateDir, 0o700); err != nil {
		_ = os.RemoveAll(stateDir)
		return nil, fmt.Errorf("secure planner session: %w", err)
	}
	return &Session{
		workdir:       workdir,
		stateDir:      stateDir,
		removeState:   true,
		completedKeys: append([]string(nil), document.Keys...),
	}, nil
}

func OpenFromEnv(workdir string) (*Session, error) {
	return openFromState(workdir, os.Getenv(plannerSessionEnv))
}

func OpenFromEnvironment(workdir string, env []string) (*Session, error) {
	stateDir := ""
	for _, entry := range env {
		key, value, ok := strings.Cut(entry, "=")
		if ok && key == plannerSessionEnv {
			stateDir = value
		}
	}
	return openFromState(workdir, stateDir)
}

func openFromState(workdir, stateDir string) (*Session, error) {
	if err := validateWorkdir(workdir); err != nil {
		return nil, err
	}
	if stateDir == "" {
		return nil, fmt.Errorf("planner session environment is missing")
	}
	info, err := os.Stat(stateDir)
	if err != nil {
		return nil, fmt.Errorf("open planner session: %w", err)
	}
	if !info.IsDir() || info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("open planner session: insecure session directory")
	}
	plan, touchset, fresh, err := inspectTargets(workdir)
	if err != nil {
		return nil, err
	}
	if fresh {
		return nil, malformedStartingError("pair", fmt.Errorf("planner targets are missing"))
	}
	document, err := parsePlan(plan)
	if err != nil {
		return nil, malformedStartingError("plan.md", err)
	}
	if _, err := parseTouchset(touchset); err != nil {
		return nil, malformedStartingError("touchset.json", err)
	}
	session := &Session{workdir: workdir, stateDir: stateDir, completedKeys: append([]string(nil), document.Keys...)}
	manifest, err := session.readManifest()
	if err != nil {
		return nil, err
	}
	if manifest != nil {
		session.manifest = manifest
	}
	return session, nil
}

func (s *Session) Env() []string {
	return []string{plannerSessionEnv + "=" + s.stateDir}
}

func (s *Session) Close() error {
	if !s.removeState || s.stateDir == "" {
		return nil
	}
	s.removeState = false
	return os.RemoveAll(s.stateDir)
}

func (s *Session) ValidateComplete() error {
	if s.manifest == nil {
		manifest, err := s.readManifest()
		if err != nil {
			return finalValidationError("pair", "", "planner manifest is invalid")
		}
		if manifest == nil {
			return finalValidationError("pair", "", "planner manifest is not initialized")
		}
		s.manifest = manifest
	}
	if err := ValidateManifest(*s.manifest); err != nil {
		return finalValidationError("pair", "", "manifest is invalid")
	}
	plan, err := os.ReadFile(filepath.Join(s.workdir, "plan.md"))
	if err != nil {
		return finalValidationError("plan.md", "", "cannot read plan")
	}
	document, err := parsePlan(plan)
	if err != nil {
		return finalValidationError("plan.md", diagnosticKey(err), "plan structure is invalid")
	}
	if err := validateDocumentAgainstManifest(document, *s.manifest, true); err != nil {
		return finalValidationError("plan.md", diagnosticKey(err), "plan sections are incomplete or out of order")
	}
	touchset, err := os.ReadFile(filepath.Join(s.workdir, "touchset.json"))
	if err != nil {
		return finalValidationError("touchset.json", "", "cannot read touchset")
	}
	set, err := parseTouchset(touchset)
	if err != nil {
		return finalValidationError("touchset.json", diagnosticKey(err), "touchset is invalid")
	}
	want := manifestGlobs(*s.manifest)
	got, err := canonicalGlobs(set.Globs)
	if err != nil {
		return finalValidationError("touchset.json", "", "touchset contains invalid globs")
	}
	if !sameStrings(got, want) {
		return finalValidationError("pair", "", "plan and touchset are inconsistent")
	}
	return nil
}

func (s *Session) manifestPath() string { return filepath.Join(s.stateDir, "manifest.json") }

func (s *Session) readManifest() (*Manifest, error) {
	file, err := os.Open(s.manifestPath())
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read planner manifest: %w", err)
	}
	defer file.Close()
	var manifest Manifest
	decoder := json.NewDecoder(file)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return nil, fmt.Errorf("read planner manifest: %w", err)
	}
	if err := ensureEOF(decoder); err != nil {
		return nil, fmt.Errorf("read planner manifest: %w", err)
	}
	if err := ValidateManifest(manifest); err != nil {
		return nil, fmt.Errorf("read planner manifest: %w", err)
	}
	return &manifest, nil
}

func ensureEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return fmt.Errorf("trailing JSON value")
		}
		return err
	}
	return nil
}

func validateWorkdir(workdir string) error {
	info, err := os.Stat(workdir)
	if err != nil {
		return fmt.Errorf("planner workdir: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("planner workdir is not a directory")
	}
	return nil
}

func inspectTargets(workdir string) ([]byte, []byte, bool, error) {
	planPath := filepath.Join(workdir, "plan.md")
	touchsetPath := filepath.Join(workdir, "touchset.json")
	planInfo, planErr := os.Lstat(planPath)
	touchsetInfo, touchsetErr := os.Lstat(touchsetPath)
	planMissing := os.IsNotExist(planErr)
	touchsetMissing := os.IsNotExist(touchsetErr)
	if planMissing && touchsetMissing {
		return nil, nil, true, nil
	}
	if planErr != nil && !planMissing {
		return nil, nil, false, malformedStartingError("plan.md", planErr)
	}
	if touchsetErr != nil && !touchsetMissing {
		return nil, nil, false, malformedStartingError("touchset.json", touchsetErr)
	}
	if planMissing || touchsetMissing {
		artifact := "plan.md"
		if planMissing {
			artifact = "plan.md"
		} else {
			artifact = "touchset.json"
		}
		return nil, nil, false, malformedStartingError(artifact, fmt.Errorf("paired target is missing"))
	}
	if planInfo.Mode()&os.ModeSymlink != 0 || !planInfo.Mode().IsRegular() {
		return nil, nil, false, malformedStartingError("plan.md", fmt.Errorf("target is not a regular file"))
	}
	if touchsetInfo.Mode()&os.ModeSymlink != 0 || !touchsetInfo.Mode().IsRegular() {
		return nil, nil, false, malformedStartingError("touchset.json", fmt.Errorf("target is not a regular file"))
	}
	plan, err := os.ReadFile(planPath)
	if err != nil {
		return nil, nil, false, malformedStartingError("plan.md", err)
	}
	touchset, err := os.ReadFile(touchsetPath)
	if err != nil {
		return nil, nil, false, malformedStartingError("touchset.json", err)
	}
	return plan, touchset, false, nil
}

func createFreshTargets(workdir string) error {
	staging, err := os.MkdirTemp("", "watchtower-planner-init-")
	if err != nil {
		return fmt.Errorf("create planner staging: %w", err)
	}
	defer os.RemoveAll(staging)
	planTemp := filepath.Join(staging, "plan.md")
	touchsetTemp := filepath.Join(staging, "touchset.json")
	if err := writeSynced(planTemp, []byte("# Implementation Plan\n\n"), 0o644); err != nil {
		return err
	}
	if err := writeSynced(touchsetTemp, []byte(`{"globs":[]}`), 0o644); err != nil {
		return err
	}
	if err := os.Rename(planTemp, filepath.Join(workdir, "plan.md")); err != nil {
		return fmt.Errorf("create plan.md: %w", err)
	}
	if err := os.Rename(touchsetTemp, filepath.Join(workdir, "touchset.json")); err != nil {
		return fmt.Errorf("create touchset.json: %w", err)
	}
	return nil
}

func writeSynced(path string, data []byte, mode os.FileMode) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("create staged artifact: %w", err)
	}
	if _, err := file.Write(data); err == nil {
		err = file.Sync()
	}
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return fmt.Errorf("write staged artifact: %w", err)
	}
	if err := os.Chmod(path, mode); err != nil {
		return fmt.Errorf("set staged artifact mode: %w", err)
	}
	return nil
}

func malformedStartingError(artifact string, err error) error {
	key := diagnosticKey(err)
	return &DiagnosticError{Scope: ScopeMalformedStartingArtifact, Artifact: artifact, Key: key, Reason: "starting artifact is invalid"}
}

func finalValidationError(artifact, key, reason string) error {
	return &DiagnosticError{Scope: ScopeFinalValidation, Artifact: artifact, Key: key, Reason: reason}
}

func diagnosticKey(err error) string {
	var diagnostic *DiagnosticError
	if err != nil && errorsAs(err, &diagnostic) {
		return diagnostic.Key
	}
	return ""
}

func errorsAs(err error, target **DiagnosticError) bool {
	if err == nil {
		return false
	}
	if diagnostic, ok := err.(*DiagnosticError); ok {
		*target = diagnostic
		return true
	}
	return false
}
