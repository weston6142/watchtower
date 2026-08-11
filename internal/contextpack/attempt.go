package contextpack

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/weston6142/watchtower/internal/stageresult"
)

// AttemptArtifact identifies immutable bytes owned by one stage attempt.
type AttemptArtifact struct {
	Name   string `json:"name"`
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

// AttemptResult is the durable model-result envelope. ResultPath points to
// its immutable manifest; the declared result files live below its result/
// directory and are copied before the envelope becomes visible.
type AttemptResult struct {
	AttemptID    string              `json:"attempt_id"`
	IssueID      string              `json:"issue_id"`
	Stage        string              `json:"stage"`
	ResultPath   string              `json:"result_path"`
	ResultSHA256 string              `json:"result_sha256"`
	Artifacts    []AttemptArtifact   `json:"artifacts"`
	DependsOn    []string            `json:"depends_on"`
	StageResult  *stageresult.Result `json:"stage_result,omitempty"`
}

type attemptManifest struct {
	TransitionID string            `json:"transition_id"`
	Artifacts    []AttemptArtifact `json:"artifacts"`
}

type resultManifest struct {
	AttemptID   string              `json:"attempt_id"`
	IssueID     string              `json:"issue_id"`
	Stage       string              `json:"stage"`
	Artifacts   []AttemptArtifact   `json:"artifacts"`
	DependsOn   []string            `json:"depends_on"`
	StageResult *stageresult.Result `json:"stage_result,omitempty"`
}

// MaterializeAttemptResult copies declared model outputs into the immutable
// result slot for attemptID. It accepts an existing exact replay and never
// replaces bytes already owned by the attempt.
func MaterializeAttemptResult(sourceDir, issueDir, attemptID string,
	names []string, metadata AttemptResult) (AttemptResult, error) {
	if !safeAttemptID(attemptID) {
		return AttemptResult{}, fmt.Errorf("unsafe attempt id %q", attemptID)
	}
	if metadata.AttemptID != "" && metadata.AttemptID != attemptID {
		return AttemptResult{}, fmt.Errorf("attempt result identity conflicts with attempt %q", attemptID)
	}
	metadata.AttemptID = attemptID
	if metadata.StageResult != nil {
		validated, err := stageresult.ValidatePersisted(*metadata.StageResult)
		if err != nil {
			return AttemptResult{}, fmt.Errorf("validate structured stage result: %w", err)
		}
		if validated.IssueID != metadata.IssueID || validated.AttemptID != metadata.AttemptID {
			return AttemptResult{}, fmt.Errorf("structured stage result identity conflicts with attempt manifest")
		}
		metadata.StageResult = &validated
	}
	metadata.ResultPath = filepath.ToSlash(filepath.Join("artifacts", "attempts", attemptID, "result", "manifest.json"))
	metadata.Artifacts = nil
	for _, name := range names {
		if err := validateName(name); err != nil {
			return AttemptResult{}, err
		}
		source := filepath.Join(sourceDir, filepath.FromSlash(name))
		info, err := os.Lstat(source)
		if err != nil {
			return AttemptResult{}, err
		}
		if !info.Mode().IsRegular() {
			return AttemptResult{}, fmt.Errorf("artifact %q is not a regular file", name)
		}
		destination := filepath.Join(issueDir, "artifacts", "attempts", attemptID, "result", filepath.FromSlash(name))
		digest, err := copyImmutable(source, destination)
		if err != nil {
			return AttemptResult{}, err
		}
		metadata.Artifacts = append(metadata.Artifacts, AttemptArtifact{
			Name: name, Path: filepath.ToSlash(filepath.Join("artifacts", "attempts", attemptID, "result", filepath.FromSlash(name))),
			SHA256: digest,
		})
	}
	sort.Slice(metadata.Artifacts, func(i, j int) bool { return metadata.Artifacts[i].Name < metadata.Artifacts[j].Name })
	manifest, err := json.Marshal(resultManifest{
		AttemptID: metadata.AttemptID, IssueID: metadata.IssueID, Stage: metadata.Stage,
		Artifacts: metadata.Artifacts, DependsOn: append([]string(nil), metadata.DependsOn...),
		StageResult: metadata.StageResult,
	})
	if err != nil {
		return AttemptResult{}, err
	}
	digest, err := writeImmutableBytes(filepath.Join(issueDir, filepath.FromSlash(metadata.ResultPath)), manifest)
	if err != nil {
		return AttemptResult{}, err
	}
	metadata.ResultSHA256 = digest
	return metadata, nil
}

// LoadAttemptResult reads the immutable result manifest for an attempt and
// validates its identity and digest before returning the declared outputs.
func LoadAttemptResult(issueDir string, expected AttemptResult) (AttemptResult, error) {
	if !safeAttemptID(expected.AttemptID) {
		return AttemptResult{}, fmt.Errorf("unsafe attempt id %q", expected.AttemptID)
	}
	wantPath := filepath.ToSlash(filepath.Join("artifacts", "attempts", expected.AttemptID, "result", "manifest.json"))
	if expected.ResultPath != wantPath || !validDigest(expected.ResultSHA256) {
		return AttemptResult{}, fmt.Errorf("invalid attempt result reference")
	}
	body, err := os.ReadFile(filepath.Join(issueDir, filepath.FromSlash(expected.ResultPath)))
	if err != nil {
		return AttemptResult{}, err
	}
	digest := sha256.Sum256(body)
	if hex.EncodeToString(digest[:]) != expected.ResultSHA256 {
		return AttemptResult{}, fmt.Errorf("attempt result manifest has a conflicting digest")
	}
	var manifest resultManifest
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return AttemptResult{}, fmt.Errorf("decode attempt result manifest: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return AttemptResult{}, fmt.Errorf("decode attempt result manifest: trailing JSON data")
	}
	if manifest.AttemptID != expected.AttemptID ||
		(expected.IssueID != "" && manifest.IssueID != expected.IssueID) ||
		(expected.Stage != "" && manifest.Stage != expected.Stage) {
		return AttemptResult{}, fmt.Errorf("attempt result manifest identity conflicts")
	}
	for _, artifact := range manifest.Artifacts {
		if err := validateName(artifact.Name); err != nil ||
			!validDigest(artifact.SHA256) ||
			!safeAttemptResultPath(artifact.Path, expected.AttemptID, artifact.Name) {
			return AttemptResult{}, fmt.Errorf("attempt result manifest contains an unsafe artifact reference")
		}
	}
	if manifest.StageResult != nil {
		validated, err := stageresult.ValidatePersisted(*manifest.StageResult)
		if err != nil {
			return AttemptResult{}, fmt.Errorf("validate structured stage result: %w", err)
		}
		if validated.IssueID != manifest.IssueID || validated.AttemptID != manifest.AttemptID {
			return AttemptResult{}, fmt.Errorf("structured stage result identity conflicts with attempt manifest")
		}
		manifest.StageResult = &validated
	}
	return AttemptResult{
		AttemptID: expected.AttemptID, IssueID: manifest.IssueID, Stage: manifest.Stage,
		ResultPath: expected.ResultPath, ResultSHA256: expected.ResultSHA256,
		Artifacts:   append([]AttemptArtifact(nil), manifest.Artifacts...),
		DependsOn:   append([]string(nil), manifest.DependsOn...),
		StageResult: manifest.StageResult,
	}, nil
}

// PublishAttemptArchive promotes result-slot bytes to immutable attempt
// archive paths and records the transition identity in the attempt manifest.
func PublishAttemptArchive(issueDir string, result AttemptResult,
	transitionID string) ([]AttemptArtifact, error) {
	if !safeAttemptID(result.AttemptID) {
		return nil, fmt.Errorf("unsafe attempt id %q", result.AttemptID)
	}
	if strings.TrimSpace(transitionID) == "" {
		return nil, fmt.Errorf("archive transition identity is empty")
	}
	refs := append([]AttemptArtifact(nil), result.Artifacts...)
	sort.Slice(refs, func(i, j int) bool { return refs[i].Name < refs[j].Name })
	for index := range refs {
		ref := refs[index]
		if err := validateName(ref.Name); err != nil {
			return nil, err
		}
		if !validDigest(ref.SHA256) {
			return nil, fmt.Errorf("artifact %q has invalid sha256", ref.Name)
		}
		if !safeAttemptResultPath(ref.Path, result.AttemptID, ref.Name) {
			return nil, fmt.Errorf("artifact %q has an unsafe result path", ref.Name)
		}
		sourcePath := filepath.Join(issueDir, filepath.FromSlash(ref.Path))
		destinationPath := filepath.Join(issueDir, "artifacts", "attempts", result.AttemptID, filepath.FromSlash(ref.Name))
		if filepath.Clean(sourcePath) != filepath.Clean(destinationPath) {
			if _, err := copyImmutable(sourcePath, destinationPath); err != nil {
				// A crash can leave the published destination while the source
				// result slot is unavailable. The immutable destination is still
				// usable when its digest proves it is the requested content.
				if !errors.Is(err, os.ErrNotExist) {
					return nil, err
				}
				if got, hashErr := fileSHA256(destinationPath); hashErr != nil || got != ref.SHA256 {
					if hashErr != nil {
						return nil, err
					}
					return nil, fmt.Errorf("archive %q exists with a conflicting digest", ref.Name)
				}
			}
		}
		if got, err := fileSHA256(destinationPath); err != nil || got != ref.SHA256 {
			if err != nil {
				return nil, err
			}
			return nil, fmt.Errorf("archive %q has a conflicting digest", ref.Name)
		}
		refs[index].Path = filepath.ToSlash(filepath.Join("artifacts", "attempts", result.AttemptID, filepath.FromSlash(ref.Name)))
	}
	manifestPath := filepath.Join(issueDir, "artifacts", "attempts", result.AttemptID, "manifest.json")
	manifest := attemptManifest{TransitionID: transitionID, Artifacts: refs}
	encoded, err := json.Marshal(manifest)
	if err != nil {
		return nil, err
	}
	if existing, readErr := os.ReadFile(manifestPath); readErr == nil {
		var prior attemptManifest
		if err := json.Unmarshal(existing, &prior); err != nil {
			return nil, fmt.Errorf("decode attempt archive manifest: %w", err)
		}
		if prior.TransitionID != transitionID || !sameAttemptArtifacts(prior.Artifacts, refs) {
			return nil, fmt.Errorf("conflicting archive replay for transition %q", transitionID)
		}
		return refs, nil
	} else if !errors.Is(readErr, os.ErrNotExist) {
		return nil, readErr
	}
	if _, err := writeImmutableBytes(manifestPath, encoded); err != nil {
		if existing, readErr := os.ReadFile(manifestPath); readErr == nil {
			var prior attemptManifest
			if jsonErr := json.Unmarshal(existing, &prior); jsonErr == nil &&
				prior.TransitionID == transitionID && sameAttemptArtifacts(prior.Artifacts, refs) {
				return refs, nil
			}
		}
		return nil, err
	}
	return refs, nil
}

// MaterializeAttemptArtifacts restores immutable archive refs into a stage
// workdir after validating each source digest.
func MaterializeAttemptArtifacts(issueDir, workdir string,
	refs []AttemptArtifact) error {
	for _, ref := range refs {
		if err := validateName(ref.Name); err != nil {
			return err
		}
		if !safeStoredPath(ref.Path) || !validDigest(ref.SHA256) {
			return fmt.Errorf("invalid attempt archive reference for %q", ref.Name)
		}
		source := filepath.Join(issueDir, filepath.FromSlash(ref.Path))
		got, err := fileSHA256(source)
		if err != nil {
			return err
		}
		if got != ref.SHA256 {
			return fmt.Errorf("attempt archive %q has digest %s, want %s", ref.Name, got, ref.SHA256)
		}
		if _, err := copyAtomic(source, filepath.Join(workdir, filepath.FromSlash(ref.Name))); err != nil {
			return err
		}
	}
	return nil
}

// MaterializeLegacy names the unchanged v1 reader explicitly for recovery
// code while retaining Materialize as the historical public behavior.
func MaterializeLegacy(issueDir, workdir string, names []string) error {
	return Materialize(issueDir, workdir, names)
}

func copyImmutable(source, destination string) (string, error) {
	input, err := os.Open(source)
	if err != nil {
		return "", err
	}
	defer input.Close()
	info, err := input.Stat()
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("source %q is not a regular file", source)
	}
	hash := sha256.New()
	if existing, statErr := os.Lstat(destination); statErr == nil {
		if !existing.Mode().IsRegular() {
			return "", fmt.Errorf("immutable destination %q is not a regular file", destination)
		}
		if _, err := io.Copy(hash, input); err != nil {
			return "", err
		}
		expected := hex.EncodeToString(hash.Sum(nil))
		got, err := fileSHA256(destination)
		if err != nil {
			return "", err
		}
		if expected != got {
			return "", fmt.Errorf("immutable destination %q has a conflicting digest", destination)
		}
		return expected, nil
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return "", statErr
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
		return "", err
	}
	temp, err := os.CreateTemp(filepath.Dir(destination), ".watchtower-attempt-*")
	if err != nil {
		return "", err
	}
	tempName := temp.Name()
	defer os.Remove(tempName)
	if _, err := input.Seek(0, io.SeekStart); err != nil {
		temp.Close()
		return "", err
	}
	hash.Reset()
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
	digest := hex.EncodeToString(hash.Sum(nil))
	if err := os.Link(tempName, destination); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return "", err
		}
		got, hashErr := fileSHA256(destination)
		if hashErr != nil {
			return "", hashErr
		}
		if got != digest {
			return "", fmt.Errorf("immutable destination %q has a conflicting digest", destination)
		}
		return digest, nil
	}
	return digest, nil
}

func writeImmutableBytes(destination string, content []byte) (string, error) {
	tempDir := filepath.Dir(destination)
	if err := os.MkdirAll(tempDir, 0o755); err != nil {
		return "", err
	}
	temp, err := os.CreateTemp(tempDir, ".watchtower-attempt-manifest-*")
	if err != nil {
		return "", err
	}
	tempName := temp.Name()
	defer os.Remove(tempName)
	if _, err := temp.Write(content); err != nil {
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
	digest := sha256.Sum256(content)
	if err := os.Link(tempName, destination); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return "", err
		}
		got, hashErr := fileSHA256(destination)
		if hashErr != nil {
			return "", hashErr
		}
		want := hex.EncodeToString(digest[:])
		if got != want {
			return "", fmt.Errorf("immutable destination %q has a conflicting digest", destination)
		}
		return want, nil
	}
	return hex.EncodeToString(digest[:]), nil
}

func fileSHA256(name string) (string, error) {
	info, err := os.Lstat(name)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("%q is not a regular file", name)
	}
	body, err := os.ReadFile(name)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(body)
	return hex.EncodeToString(digest[:]), nil
}

func safeAttemptID(value string) bool {
	return value != "" && value != "." && value != ".." &&
		!strings.ContainsAny(value, `/\\`) && !strings.Contains(value, "\x00")
}

func safeStoredPath(value string) bool {
	if value == "" || strings.ContainsAny(value, `\\`) || strings.HasPrefix(value, "/") {
		return false
	}
	clean := path.Clean(value)
	return clean == value && !strings.HasPrefix(clean, "../") && strings.HasPrefix(clean, "artifacts/attempts/")
}

func safeAttemptResultPath(value, attemptID, name string) bool {
	want := path.Join("artifacts", "attempts", attemptID, "result", name)
	return safeStoredPath(value) && value == want
}

func validDigest(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, char := range value {
		if !(char >= '0' && char <= '9') && !(char >= 'a' && char <= 'f') {
			return false
		}
	}
	return true
}

func sameAttemptArtifacts(left, right []AttemptArtifact) bool {
	left = append([]AttemptArtifact(nil), left...)
	right = append([]AttemptArtifact(nil), right...)
	sort.Slice(left, func(i, j int) bool { return left[i].Name < left[j].Name })
	sort.Slice(right, func(i, j int) bool { return right[i].Name < right[j].Name })
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
