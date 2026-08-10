package plannerartifact

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Binding identifies one stage attempt. Every field participates in registry
// lookup and verification; no field is an optional selector.
type Binding struct {
	IssueID  string
	Stage    string
	Attempt  int
	Worktree string
}

// Registry is the narrow coordinator-store contract used by the authority.
// The store owns persistence; planner artifacts never select a registry row.
type Registry interface {
	LoadPlannerArtifact(issueID, stage string, attempt int, worktree string) (status string, digest, manifest, sections []byte, found bool, err error)
	CreatePlannerArtifact(issueID, stage string, attempt int, worktree, status string, digest, manifest, sections []byte) error
	UpdatePlannerArtifact(issueID, stage string, attempt int, worktree, status string, digest, manifest, sections []byte) error
	ExpirePlannerArtifact(issueID, stage string, attempt int, worktree string) error
}

type acceptedSection struct {
	Key      string   `json:"key"`
	Markdown string   `json:"markdown"`
	Globs    []string `json:"globs"`
}

type authorityRecord struct {
	Manifest Manifest
	Sections []acceptedSection
}

// Authority is the engine-issued, in-process handle to one durable planner
// record. Its capability is intentionally private and has no serialization or
// environment representation.
type Authority struct {
	registry   Registry
	binding    Binding
	digest     []byte
	capability string
	mu         sync.Mutex
	closed     bool
}

const capabilityBytes = 32

// CreateOrLoad creates or reloads the exact bound record and issues a fresh
// opaque capability for the live engine context.
func CreateOrLoad(registry Registry, binding Binding) (*Authority, error) {
	if registry == nil {
		return nil, errors.New("planner authority: registry is unavailable")
	}
	normalized, err := normalizeBinding(binding)
	if err != nil {
		return nil, err
	}
	if err := Prepare(normalized.Worktree); err != nil {
		return nil, err
	}
	capability := make([]byte, capabilityBytes)
	if _, err := rand.Read(capability); err != nil {
		return nil, fmt.Errorf("planner authority: generate capability: %w", err)
	}
	digest := capabilityDigest(capability)
	status, storedDigest, manifest, sections, found, err := registry.LoadPlannerArtifact(
		normalized.IssueID, normalized.Stage, normalized.Attempt, normalized.Worktree)
	if err != nil {
		return nil, fmt.Errorf("planner authority: registry read failed")
	}
	if found {
		if status != "active" {
			return nil, fmt.Errorf("planner authority: record is not active")
		}
		if _, err := decodeRecord(storedDigest, manifest, sections); err != nil {
			return nil, err
		}
		if err := registry.UpdatePlannerArtifact(normalized.IssueID, normalized.Stage, normalized.Attempt, normalized.Worktree,
			"active", digest, manifest, normalizeSectionsJSON(sections)); err != nil {
			return nil, fmt.Errorf("planner authority: registry write failed")
		}
	} else {
		if err := registry.CreatePlannerArtifact(normalized.IssueID, normalized.Stage, normalized.Attempt, normalized.Worktree,
			"active", digest, nil, []byte("[]")); err != nil {
			return nil, fmt.Errorf("planner authority: registry write failed")
		}
	}
	return &Authority{
		registry: registry, binding: normalized, digest: digest,
		capability: base64.RawURLEncoding.EncodeToString(capability),
	}, nil
}

// CapabilityHandle is available only to the engine/daemon route. Runners and
// provider interfaces never receive this value, and it is never persisted.
func (a *Authority) CapabilityHandle() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.capability
}

// VerifyCapability checks a presented opaque handle without exposing the
// durable digest or the raw value in an error.
func (a *Authority) VerifyCapability(handle string) error {
	capability, err := base64.RawURLEncoding.DecodeString(handle)
	if err != nil || len(capability) != capabilityBytes {
		return authorityError(ErrorStaleCapability, "", "capability is not active", nil)
	}
	digest := capabilityDigest(capability)
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.verifyLocked(); err != nil {
		return err
	}
	if subtle.ConstantTimeCompare(digest, a.digest) != 1 {
		return authorityError(ErrorStaleCapability, "", "capability is not active", nil)
	}
	return nil
}

// ApplyWithCapability is the daemon-facing authority entry point. The
// capability is checked before the provider-neutral request reaches Apply.
func (a *Authority) ApplyWithCapability(handle string, request WriteRequest) error {
	if err := a.VerifyCapability(handle); err != nil {
		return err
	}
	return a.Apply(request)
}

func normalizeBinding(binding Binding) (Binding, error) {
	if binding.IssueID == "" || binding.Stage == "" || binding.Attempt <= 0 || binding.Worktree == "" {
		return Binding{}, errors.New("planner authority: binding is incomplete")
	}
	abs, err := filepath.Abs(binding.Worktree)
	if err != nil {
		return Binding{}, errors.New("planner authority: worktree is invalid")
	}
	canonical, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return Binding{}, errors.New("planner authority: worktree is unavailable")
	}
	return Binding{IssueID: binding.IssueID, Stage: binding.Stage, Attempt: binding.Attempt, Worktree: canonical}, nil
}

func capabilityDigest(capability []byte) []byte {
	digest := sha256.Sum256(capability)
	return digest[:]
}

func (a *Authority) Binding() Binding {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.binding
}

// VerifyBinding rejects a caller attempting to use this handle for any other
// exact tuple before it can reach validation or file mutation.
func (a *Authority) VerifyBinding(binding Binding) error {
	normalized, err := normalizeBinding(binding)
	if err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if normalized != a.binding {
		return authorityError(ErrorScopeMismatch, "", "binding mismatch", nil)
	}
	return a.verifyLocked()
}

func (a *Authority) verifyLocked() error {
	if a.closed {
		return authorityError(ErrorStaleCapability, "", "authority is closed", nil)
	}
	status, storedDigest, _, _, found, err := a.registry.LoadPlannerArtifact(
		a.binding.IssueID, a.binding.Stage, a.binding.Attempt, a.binding.Worktree)
	if err != nil {
		return authorityError(ErrorAuthorityState, "", "registry read failed", err)
	}
	if !found || status != "active" || len(storedDigest) == 0 ||
		subtle.ConstantTimeCompare(storedDigest, a.digest) != 1 {
		return authorityError(ErrorStaleCapability, "", "authority is not active", nil)
	}
	return nil
}

// Apply validates and durably advances one provider-neutral planner section.
func (a *Authority) Apply(request WriteRequest) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.verifyLocked(); err != nil {
		return err
	}
	status, digest, manifestBytes, sectionBytes, found, err := a.registry.LoadPlannerArtifact(
		a.binding.IssueID, a.binding.Stage, a.binding.Attempt, a.binding.Worktree)
	if err != nil || !found || status != "active" || subtle.ConstantTimeCompare(digest, a.digest) != 1 {
		return authorityError(ErrorAuthorityState, request.Key, "registry state is unavailable", err)
	}
	record, err := decodeRecord(digest, manifestBytes, sectionBytes)
	if err != nil {
		return err
	}
	manifest, err := normalizedManifest(request.Manifest)
	if err != nil {
		return authorityError(ErrorInvalidSection, request.Key, "request manifest is invalid", err)
	}
	if len(manifestBytes) != 0 && string(manifestBytes) != "null" && string(manifestBytes) != "" && !manifestsEqual(record.Manifest, manifest) {
		return authorityError(ErrorInvalidSection, request.Key, "request manifest changed", nil)
	}
	entryIndex := -1
	for i, entry := range manifest.Sections {
		if entry.Key == request.Key {
			entryIndex = i
			break
		}
	}
	if entryIndex < 0 {
		return authorityError(ErrorInvalidSection, request.Key, "request key is not in manifest", nil)
	}
	requestGlobs, err := canonicalGlobs(request.Globs)
	if err != nil || !sameStrings(requestGlobs, manifest.Sections[entryIndex].Globs) {
		return authorityError(ErrorInvalidSection, request.Key, "request globs do not match manifest", err)
	}
	if err := validateFinalSection(request.Markdown); err != nil {
		return authorityError(ErrorInvalidSection, request.Key, "request section is invalid", err)
	}
	deltaBytes, err := json.Marshal(manifest.Sections[entryIndex].Globs)
	if err != nil {
		return errors.New("planner authority: encode section delta")
	}
	if observed := len([]byte(request.Markdown)) + len(deltaBytes); observed > MaxOperationBytes {
		return authorityError(ErrorInvalidSection, request.Key, "operation exceeds bounded write", nil)
	}
	if len(record.Manifest.Sections) == 0 {
		record.Manifest = manifest
	}
	if entryIndex < len(record.Sections) {
		accepted := record.Sections[entryIndex]
		if accepted.Key != request.Key || accepted.Markdown != normalizedMarkdown(request.Markdown) || !sameStrings(accepted.Globs, requestGlobs) {
			return authorityError(ErrorInvalidSection, request.Key, "accepted section conflicts with replay", nil)
		}
		return nil
	}
	if entryIndex != len(record.Sections) {
		return authorityError(ErrorInvalidSection, request.Key, "request skips a pending section", nil)
	}
	candidate := append(append([]acceptedSection(nil), record.Sections...), acceptedSection{
		Key: request.Key, Markdown: normalizedMarkdown(request.Markdown), Globs: append([]string(nil), requestGlobs...),
	})
	priorPlan, err := os.ReadFile(filepath.Join(a.binding.Worktree, "plan.md"))
	if err != nil {
		return authorityError(ErrorAuthorityState, request.Key, "plan state is unavailable", err)
	}
	priorTouchset, err := os.ReadFile(filepath.Join(a.binding.Worktree, "touchset.json"))
	if err != nil {
		return authorityError(ErrorAuthorityState, request.Key, "touchset state is unavailable", err)
	}
	if err := a.publishAccepted(candidate); err != nil {
		_ = a.restorePair(priorPlan, priorTouchset)
		return authorityError(ErrorAuthorityState, request.Key, "pair publication failed", err)
	}
	encodedManifest, err := json.Marshal(record.Manifest)
	if err != nil {
		_ = a.restorePair(priorPlan, priorTouchset)
		return authorityError(ErrorAuthorityState, request.Key, "manifest state is unavailable", err)
	}
	encodedSections, err := json.Marshal(candidate)
	if err != nil {
		_ = a.restorePair(priorPlan, priorTouchset)
		return authorityError(ErrorAuthorityState, request.Key, "section state is unavailable", err)
	}
	if err := a.registry.UpdatePlannerArtifact(a.binding.IssueID, a.binding.Stage, a.binding.Attempt, a.binding.Worktree,
		"active", a.digest, encodedManifest, encodedSections); err != nil {
		_ = a.restorePair(priorPlan, priorTouchset)
		return authorityError(ErrorAuthorityState, request.Key, "registry write failed", err)
	}
	return nil
}

// ApplyPlannerArtifact is the typed-runner adapter. Unknown values are
// rejected, so an agent cannot substitute a private session object.
func (a *Authority) ApplyPlannerArtifact(value any) error {
	request, ok := value.(WriteRequest)
	if !ok {
		if pointer, pointerOK := value.(*WriteRequest); pointerOK && pointer != nil {
			request, ok = *pointer, true
		}
	}
	if !ok {
		if text, textOK := value.(string); textOK && strings.Contains(text, "WATCHTOWER_PLANNER_SESSION") {
			return authorityError(ErrorPrivateSession, "", "private session is not authoritative", nil)
		}
		return authorityError(ErrorDescriptor, "", "descriptor or private value is not authoritative", nil)
	}
	return a.Apply(request)
}

func (a *Authority) publishAccepted(sections []acceptedSection) error {
	plan, touchset, err := renderPair(sections)
	if err != nil {
		return err
	}
	session := &Session{workdir: a.binding.Worktree}
	if err := session.publishPair(plan, touchset, ""); err != nil {
		return err
	}
	return nil
}

func (a *Authority) restorePair(plan, touchset []byte) error {
	session := &Session{workdir: a.binding.Worktree}
	return session.publishPair(plan, touchset, "")
}

func renderPair(sections []acceptedSection) ([]byte, []byte, error) {
	plan := []byte(planRoot)
	var globs []string
	for _, section := range sections {
		if !validKey(section.Key) {
			return nil, nil, errors.New("planner authority: stored section is invalid")
		}
		plan = append(plan, []byte("<!-- watchtower-section: key="+section.Key+" -->\n")...)
		plan = append(plan, []byte(normalizedMarkdown(section.Markdown))...)
		plan = append(plan, []byte("<!-- watchtower-section-end: key="+section.Key+" -->\n")...)
		globs = append(globs, section.Globs...)
	}
	canonical, err := canonicalGlobs(globs)
	if err != nil {
		return nil, nil, errors.New("planner authority: stored touchset is invalid")
	}
	touchset, err := json.Marshal(touchsetDocument{Globs: canonical})
	if err != nil {
		return nil, nil, errors.New("planner authority: encode touchset")
	}
	return plan, touchset, nil
}

func decodeRecord(digest, manifestBytes, sectionsBytes []byte) (authorityRecord, error) {
	if len(digest) == 0 {
		return authorityRecord{}, errors.New("planner authority: record digest is missing")
	}
	record := authorityRecord{}
	if len(manifestBytes) != 0 && string(manifestBytes) != "null" {
		if err := json.Unmarshal(manifestBytes, &record.Manifest); err != nil {
			return authorityRecord{}, errors.New("planner authority: record manifest is malformed")
		}
		if err := ValidateManifest(record.Manifest); err != nil {
			return authorityRecord{}, errors.New("planner authority: record manifest is invalid")
		}
	}
	if len(sectionsBytes) == 0 {
		sectionsBytes = []byte("[]")
	}
	if err := json.Unmarshal(sectionsBytes, &record.Sections); err != nil {
		return authorityRecord{}, errors.New("planner authority: record sections are malformed")
	}
	return record, nil
}

func normalizeSectionsJSON(sections []byte) []byte {
	if len(sections) == 0 {
		return []byte("[]")
	}
	return sections
}

// ValidateComplete applies the existing complete-pair gate to the durable
// manifest associated with this authority.
func (a *Authority) ValidateComplete() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.verifyLocked(); err != nil {
		return err
	}
	_, digest, manifestBytes, sections, found, err := a.registry.LoadPlannerArtifact(
		a.binding.IssueID, a.binding.Stage, a.binding.Attempt, a.binding.Worktree)
	if err != nil || !found || subtle.ConstantTimeCompare(digest, a.digest) != 1 {
		return errors.New("planner authority: registry state is unavailable")
	}
	record, err := decodeRecord(digest, manifestBytes, sections)
	if err != nil || len(record.Manifest.Sections) == 0 {
		return finalValidationError("pair", "", "planner manifest is not initialized")
	}
	if len(record.Sections) != len(record.Manifest.Sections) {
		return finalValidationError("pair", "", "planner sections are incomplete")
	}
	for index, entry := range record.Manifest.Sections {
		accepted := record.Sections[index]
		if accepted.Key != entry.Key || !sameStrings(accepted.Globs, entry.Globs) {
			return finalValidationError("pair", entry.Key, "durable planner sections are inconsistent")
		}
	}
	session := &Session{workdir: a.binding.Worktree, manifest: &record.Manifest}
	if err := session.ValidateComplete(); err != nil {
		return err
	}
	expectedPlan, expectedTouchset, err := renderPair(record.Sections)
	if err != nil {
		return finalValidationError("pair", "", "durable planner sections cannot be rendered")
	}
	actualPlan, err := os.ReadFile(filepath.Join(a.binding.Worktree, "plan.md"))
	if err != nil || !bytes.Equal(actualPlan, expectedPlan) {
		return finalValidationError("plan.md", "", "planner content does not match durable validated state")
	}
	actualTouchset, err := os.ReadFile(filepath.Join(a.binding.Worktree, "touchset.json"))
	if err != nil || !bytes.Equal(actualTouchset, expectedTouchset) {
		return finalValidationError("touchset.json", "", "touchset does not match durable validated state")
	}
	return nil
}

// AcceptedSections reloads the durable validated prefix for retry inspection.
func (a *Authority) AcceptedSections() ([]WriteRequest, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.verifyLocked(); err != nil {
		return nil, err
	}
	_, digest, manifestBytes, sections, found, err := a.registry.LoadPlannerArtifact(
		a.binding.IssueID, a.binding.Stage, a.binding.Attempt, a.binding.Worktree)
	if err != nil || !found {
		return nil, errors.New("planner authority: registry state is unavailable")
	}
	record, err := decodeRecord(digest, manifestBytes, sections)
	if err != nil {
		return nil, err
	}
	requests := make([]WriteRequest, 0, len(record.Sections))
	for _, section := range record.Sections {
		requests = append(requests, WriteRequest{Manifest: record.Manifest, Key: section.Key, Markdown: section.Markdown, Globs: append([]string(nil), section.Globs...)})
	}
	return requests, nil
}

// AttachPlannerArtifactDescriptor remains only as a compatibility symbol for
// older callers. Descriptors are never authoritative and cannot mutate plan
// state.
func (a *Authority) AttachPlannerArtifactDescriptor() (*os.File, error) {
	return nil, authorityError(ErrorDescriptor, "", "descriptor is not authoritative", nil)
}

// ApplyFromFD remains only as a compatibility symbol. It always fails closed;
// the daemon route is the only authority transport.
func ApplyFromFD(fd int, request WriteRequest) error {
	return authorityError(ErrorDescriptor, request.Key, "descriptor is not authoritative", nil)
}

// Expire closes the authority and retains the historical registry state.
func (a *Authority) Expire() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return nil
	}
	if err := a.registry.ExpirePlannerArtifact(a.binding.IssueID, a.binding.Stage, a.binding.Attempt, a.binding.Worktree); err != nil {
		return err
	}
	a.closed = true
	return nil
}

// Close releases the live handle without expiring durable progress. Failed
// stage attempts can therefore reload the same binding on retry.
func (a *Authority) Close() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.closed = true
	return nil
}
