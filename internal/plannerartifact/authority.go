package plannerartifact

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
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

type retryRegistry interface {
	LoadLatestPlannerArtifactBefore(issueID, stage string, beforeAttempt int, worktree string) (attempt int, status string, digest, manifest, sections []byte, found bool, err error)
}

type retryPromotionRegistry interface {
	PromotePlannerArtifact(issueID, stage string, priorAttempt, attempt int, worktree string, digest, manifest, sections []byte) error
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

// CreateOrLoadOption configures authority recovery without changing the
// production defaults.
type CreateOrLoadOption func(*createOrLoadConfig)

type createOrLoadConfig struct {
	requireDurable bool
	rename         func(string, string) error
	random         io.Reader
}

// RequireDurableRecovery rejects a missing exact or bounded-prior durable
// record. Daemon restart paths use this to prevent a workspace from becoming
// authority merely because its files exist.
func RequireDurableRecovery() CreateOrLoadOption {
	return func(config *createOrLoadConfig) {
		config.requireDurable = true
	}
}

// withRecoveryRename is a deterministic test seam for pair publication. The
// production path always defaults to os.Rename.
func withRecoveryRename(rename func(string, string) error) CreateOrLoadOption {
	return func(config *createOrLoadConfig) {
		config.rename = rename
	}
}

// withCapabilityReader is a deterministic test seam for capability rotation.
// The production path always defaults to crypto/rand.Reader.
func withCapabilityReader(reader io.Reader) CreateOrLoadOption {
	return func(config *createOrLoadConfig) {
		config.random = reader
	}
}

type bindingLockEntry struct {
	mu   sync.Mutex
	refs int
}

type authorityScope struct {
	IssueID  string
	Stage    string
	Worktree string
}

var authorityBindingLocks = struct {
	sync.Mutex
	entries map[authorityScope]*bindingLockEntry
}{entries: make(map[authorityScope]*bindingLockEntry)}

// acquireBindingLock counts callers before they wait so an entry is removed
// only after its final holder or waiter has released it.
func acquireBindingLock(binding Binding) func() {
	scope := authorityScope{IssueID: binding.IssueID, Stage: binding.Stage, Worktree: binding.Worktree}
	authorityBindingLocks.Lock()
	entry := authorityBindingLocks.entries[scope]
	if entry == nil {
		entry = &bindingLockEntry{}
		authorityBindingLocks.entries[scope] = entry
	}
	entry.refs++
	authorityBindingLocks.Unlock()

	entry.mu.Lock()
	return func() {
		entry.mu.Unlock()
		authorityBindingLocks.Lock()
		entry.refs--
		if entry.refs == 0 {
			delete(authorityBindingLocks.entries, scope)
		}
		authorityBindingLocks.Unlock()
	}
}

type durableAuthoritySelection struct {
	exact    bool
	attempt  int
	status   string
	digest   []byte
	manifest []byte
	sections []byte
	record   authorityRecord
}

// CreateOrLoad creates or reloads the exact bound record and issues a fresh
// opaque capability for the live engine context.
func CreateOrLoad(registry Registry, binding Binding, options ...CreateOrLoadOption) (*Authority, error) {
	if registry == nil {
		return nil, errors.New("planner authority: registry is unavailable")
	}
	normalized, err := normalizeBinding(binding)
	if err != nil {
		return nil, err
	}
	config := createOrLoadConfig{rename: os.Rename, random: rand.Reader}
	for _, option := range options {
		if option != nil {
			option(&config)
		}
	}
	release := acquireBindingLock(normalized)
	defer release()

	selection, found, err := loadDurableAuthoritySelection(registry, normalized)
	if err != nil {
		return nil, err
	}
	if !found {
		if config.requireDurable {
			return nil, authorityError(ErrorAuthorityState, "", "durable planner authority is unavailable", nil)
		}
		if err := Prepare(normalized.Worktree); err != nil {
			return nil, err
		}
		capability, digest, err := generateCapability(config.random)
		if err != nil {
			return nil, authorityError(ErrorAuthorityState, "", "capability generation failed", err)
		}
		if err := registry.CreatePlannerArtifact(normalized.IssueID, normalized.Stage, normalized.Attempt, normalized.Worktree,
			"active", digest, nil, []byte("[]")); err != nil {
			return nil, authorityError(ErrorAuthorityState, "", "registry write failed", err)
		}
		return newAuthority(registry, normalized, digest, capability), nil
	}

	if len(selection.record.Sections) == 0 {
		if err := Prepare(normalized.Worktree); err != nil {
			return nil, err
		}
		if err := validateDurablePair(normalized.Worktree, selection.record); err != nil {
			return nil, authorityError(ErrorAuthorityState, "", "durable planner pair is invalid", err)
		}
		capability, digest, err := generateCapability(config.random)
		if err != nil {
			return nil, authorityError(ErrorAuthorityState, "", "capability generation failed", err)
		}
		if err := persistReloadedAuthority(registry, normalized, selection, digest); err != nil {
			return nil, authorityError(ErrorAuthorityState, "", "registry write failed", err)
		}
		return newAuthority(registry, normalized, digest, capability), nil
	}

	plan, touchset, err := renderPair(selection.record.Sections)
	if err != nil {
		return nil, authorityError(ErrorAuthorityState, "", "durable planner state cannot be rendered", err)
	}
	recovery, err := recoverDurablePairWithRename(normalized.Worktree, plan, touchset, config.rename)
	if err != nil {
		return nil, authorityError(ErrorAuthorityState, "", "durable planner pair recovery failed", err)
	}
	defer func() {
		_ = recovery.Rollback()
	}()
	if err := Prepare(normalized.Worktree); err != nil {
		return nil, authorityError(ErrorAuthorityState, "", "recovered planner targets are invalid", err)
	}
	if err := validateDurablePair(normalized.Worktree, selection.record); err != nil {
		return nil, authorityError(ErrorAuthorityState, "", "recovered planner pair is invalid", err)
	}
	capability, digest, err := generateCapability(config.random)
	if err != nil {
		return nil, authorityError(ErrorAuthorityState, "", "capability generation failed", err)
	}
	if err := persistReloadedAuthority(registry, normalized, selection, digest); err != nil {
		return nil, authorityError(ErrorAuthorityState, "", "registry write failed", err)
	}
	recovery.Commit()
	return newAuthority(registry, normalized, digest, capability), nil
}

func loadDurableAuthoritySelection(registry Registry, binding Binding) (durableAuthoritySelection, bool, error) {
	status, digest, manifest, sections, found, err := registry.LoadPlannerArtifact(
		binding.IssueID, binding.Stage, binding.Attempt, binding.Worktree)
	if err != nil {
		return durableAuthoritySelection{}, false, authorityError(ErrorAuthorityState, "", "registry read failed", err)
	}
	selection := durableAuthoritySelection{exact: true, attempt: binding.Attempt, status: status, digest: digest, manifest: manifest, sections: sections}
	if !found {
		retry, ok := registry.(retryRegistry)
		if !ok {
			return durableAuthoritySelection{}, false, nil
		}
		attempt, priorStatus, priorDigest, priorManifest, priorSections, priorFound, priorErr := retry.LoadLatestPlannerArtifactBefore(
			binding.IssueID, binding.Stage, binding.Attempt, binding.Worktree)
		if priorErr != nil {
			return durableAuthoritySelection{}, false, authorityError(ErrorAuthorityState, "", "planner retry state is unavailable", priorErr)
		}
		if !priorFound {
			return durableAuthoritySelection{}, false, nil
		}
		selection = durableAuthoritySelection{attempt: attempt, status: priorStatus, digest: priorDigest, manifest: priorManifest, sections: priorSections}
		if attempt <= 0 || attempt >= binding.Attempt {
			return durableAuthoritySelection{}, false, authorityError(ErrorAuthorityState, "", "planner retry state contradicts the requested attempt", nil)
		}
	}
	if selection.status != "active" {
		return durableAuthoritySelection{}, false, authorityError(ErrorAuthorityState, "", "durable planner authority is not active", nil)
	}
	record, err := decodeRecord(selection.digest, selection.manifest, selection.sections)
	if err != nil {
		return durableAuthoritySelection{}, false, authorityError(ErrorAuthorityState, "", "durable authority record is invalid", err)
	}
	if err := validateDurableRecord(record); err != nil {
		return durableAuthoritySelection{}, false, authorityError(ErrorAuthorityState, "", "durable authority record is invalid", err)
	}
	selection.record = record
	return selection, true, nil
}

func persistReloadedAuthority(registry Registry, binding Binding, selection durableAuthoritySelection, digest []byte) error {
	if selection.exact {
		return registry.UpdatePlannerArtifact(binding.IssueID, binding.Stage, binding.Attempt, binding.Worktree,
			"active", digest, selection.manifest, selection.sections)
	}
	promoter, ok := registry.(retryPromotionRegistry)
	if !ok {
		return errors.New("planner authority: atomic retry promotion is unavailable")
	}
	return promoter.PromotePlannerArtifact(binding.IssueID, binding.Stage, selection.attempt, binding.Attempt,
		binding.Worktree, digest, selection.manifest, selection.sections)
}

func generateCapability(reader io.Reader) (string, []byte, error) {
	if reader == nil {
		return "", nil, errors.New("planner authority: capability source is unavailable")
	}
	raw := make([]byte, capabilityBytes)
	if _, err := io.ReadFull(reader, raw); err != nil {
		return "", nil, err
	}
	return base64.RawURLEncoding.EncodeToString(raw), capabilityDigest(raw), nil
}

func newAuthority(registry Registry, binding Binding, digest []byte, capability string) *Authority {
	return &Authority{registry: registry, binding: binding, digest: append([]byte(nil), digest...), capability: capability}
}

// CapabilityHandle is available only to the engine/daemon route. Runners and
// provider interfaces never receive this value, and it is never persisted.
func (a *Authority) CapabilityHandle() string {
	release := acquireBindingLock(a.binding)
	defer release()
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.capability
}

// VerifyCapability checks a presented opaque handle without exposing the
// durable digest or the raw value in an error.
func (a *Authority) VerifyCapability(handle string) error {
	release := acquireBindingLock(a.binding)
	defer release()
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.verifyCapabilityLocked(handle)
}

func (a *Authority) verifyCapabilityLocked(handle string) error {
	capability, err := base64.RawURLEncoding.DecodeString(handle)
	if err != nil || len(capability) != capabilityBytes {
		return authorityError(ErrorStaleCapability, "", "capability is not active", nil)
	}
	if err := a.verifyLocked(); err != nil {
		return err
	}
	digest := capabilityDigest(capability)
	if subtle.ConstantTimeCompare(digest, a.digest) != 1 {
		return authorityError(ErrorStaleCapability, "", "capability is not active", nil)
	}
	return nil
}

// ApplyWithCapability is the daemon-facing authority entry point. The
// capability is checked before the provider-neutral request reaches Apply.
func (a *Authority) ApplyWithCapability(handle string, request WriteRequest) error {
	if strings.Contains(handle, "WATCHTOWER_PLANNER_SESSION") {
		return authorityError(ErrorPrivateSession, "", "private session is not authoritative", nil)
	}
	release := acquireBindingLock(a.binding)
	defer release()
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.verifyCapabilityLocked(handle); err != nil {
		return err
	}
	return a.applyVerifiedLocked(request)
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
	release := acquireBindingLock(a.binding)
	defer release()
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
	if !found || status != "active" || len(storedDigest) != sha256.Size ||
		subtle.ConstantTimeCompare(storedDigest, a.digest) != 1 {
		return authorityError(ErrorStaleCapability, "", "authority is not active", nil)
	}
	return nil
}

// Apply validates and durably advances one provider-neutral planner section.
func (a *Authority) Apply(request WriteRequest) error {
	release := acquireBindingLock(a.binding)
	defer release()
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.verifyLocked(); err != nil {
		return err
	}
	return a.applyVerifiedLocked(request)
}

func (a *Authority) applyVerifiedLocked(request WriteRequest) error {
	status, digest, manifestBytes, sectionBytes, found, err := a.registry.LoadPlannerArtifact(
		a.binding.IssueID, a.binding.Stage, a.binding.Attempt, a.binding.Worktree)
	if err != nil || !found || status != "active" || len(digest) != sha256.Size || subtle.ConstantTimeCompare(digest, a.digest) != 1 {
		return authorityError(ErrorAuthorityState, "", "registry state is unavailable", err)
	}
	record, err := decodeRecord(digest, manifestBytes, sectionBytes)
	if err != nil {
		return authorityError(ErrorAuthorityState, "", "durable authority record is invalid", err)
	}
	if err := validateDurableRecord(record); err != nil {
		return authorityError(ErrorAuthorityState, "", "durable authority record is invalid", err)
	}
	if err := validateDurablePair(a.binding.Worktree, record); err != nil {
		return err
	}
	manifest, err := normalizedManifest(request.Manifest)
	if err != nil {
		return authorityError(ErrorInvalidSection, "", "request manifest is invalid", err)
	}
	if len(manifestBytes) != 0 && string(manifestBytes) != "null" && string(manifestBytes) != "" && !manifestsEqual(record.Manifest, manifest) {
		return authorityError(ErrorInvalidSection, "", "request manifest changed", nil)
	}
	entryIndex := -1
	for i, entry := range manifest.Sections {
		if entry.Key == request.Key {
			entryIndex = i
			break
		}
	}
	if entryIndex < 0 {
		return authorityError(ErrorInvalidSection, "", "request key is not in manifest", nil)
	}
	sectionKey := manifest.Sections[entryIndex].Key
	requestGlobs, err := canonicalGlobs(request.Globs)
	if err != nil || !sameStrings(requestGlobs, manifest.Sections[entryIndex].Globs) {
		return authorityError(ErrorInvalidSection, sectionKey, "request globs do not match manifest", err)
	}
	if err := validateFinalSection(request.Markdown); err != nil {
		return authorityError(ErrorInvalidSection, sectionKey, "request section is invalid", err)
	}
	deltaBytes, err := json.Marshal(manifest.Sections[entryIndex].Globs)
	if err != nil {
		return errors.New("planner authority: encode section delta")
	}
	if observed := len([]byte(normalizedMarkdown(request.Markdown))) + len(deltaBytes); observed > MaxOperationBytes {
		return authorityError(ErrorInvalidSection, sectionKey, "operation exceeds bounded write", nil)
	}
	if len(record.Manifest.Sections) == 0 {
		record.Manifest = manifest
	}
	if entryIndex < len(record.Sections) {
		accepted := record.Sections[entryIndex]
		if accepted.Key != request.Key || accepted.Markdown != normalizedMarkdown(request.Markdown) || !sameStrings(accepted.Globs, requestGlobs) {
			return authorityError(ErrorInvalidSection, sectionKey, "accepted section conflicts with replay", nil)
		}
		return nil
	}
	if entryIndex != len(record.Sections) {
		return authorityError(ErrorInvalidSection, sectionKey, "request skips a pending section", nil)
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
	if len(digest) != sha256.Size {
		return authorityRecord{}, errors.New("planner authority: record digest is invalid")
	}
	record := authorityRecord{}
	trimmedManifest := bytes.TrimSpace(manifestBytes)
	if len(trimmedManifest) != 0 && !bytes.Equal(trimmedManifest, []byte("null")) {
		if err := decodeAuthorityJSON(manifestBytes, &record.Manifest); err != nil {
			return authorityRecord{}, errors.New("planner authority: record manifest is malformed")
		}
		if err := ValidateManifest(record.Manifest); err != nil {
			return authorityRecord{}, errors.New("planner authority: record manifest is invalid")
		}
	}
	if len(sectionsBytes) == 0 {
		sectionsBytes = []byte("[]")
	}
	if bytes.Equal(bytes.TrimSpace(sectionsBytes), []byte("null")) {
		return authorityRecord{}, errors.New("planner authority: record sections are malformed")
	}
	if err := decodeAuthorityJSON(sectionsBytes, &record.Sections); err != nil {
		return authorityRecord{}, errors.New("planner authority: record sections are malformed")
	}
	return record, nil
}

func decodeAuthorityJSON(data []byte, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	return ensureEOF(decoder)
}

func validateDurableRecord(record authorityRecord) error {
	if len(record.Manifest.Sections) == 0 {
		if len(record.Sections) != 0 {
			return errors.New("accepted sections exist without a manifest")
		}
		return nil
	}
	if len(record.Sections) > len(record.Manifest.Sections) {
		return errors.New("accepted sections exceed the manifest")
	}
	for _, entry := range record.Manifest.Sections {
		canonical, err := canonicalGlobs(entry.Globs)
		if err != nil || !sameStrings(canonical, entry.Globs) {
			return errors.New("manifest globs are not canonical")
		}
	}
	for index, section := range record.Sections {
		entry := record.Manifest.Sections[index]
		if section.Key != entry.Key || !sameStrings(section.Globs, entry.Globs) {
			return errors.New("accepted section does not match the manifest")
		}
		canonical, err := canonicalGlobs(section.Globs)
		if err != nil || !sameStrings(canonical, section.Globs) {
			return errors.New("accepted section globs are not canonical")
		}
		if err := validateFinalSection(section.Markdown); err != nil {
			return errors.New("accepted section content is invalid")
		}
		if normalizedMarkdown(section.Markdown) != section.Markdown {
			return errors.New("accepted section content is not normalized")
		}
		delta, err := json.Marshal(entry.Globs)
		if err != nil || len([]byte(section.Markdown))+len(delta) > MaxOperationBytes {
			return errors.New("accepted section exceeds the bounded write")
		}
	}
	plan, touchset, err := renderPair(record.Sections)
	if err != nil {
		return errors.New("accepted sections cannot be rendered")
	}
	document, err := parsePlan(plan)
	if err != nil || len(document.Sections) != len(record.Sections) {
		return errors.New("rendered plan is invalid")
	}
	if err := validateDocumentAgainstManifest(document, record.Manifest, false); err != nil {
		return errors.New("rendered plan is not a manifest prefix")
	}
	for index := range document.Sections {
		if document.Sections[index].Markdown != record.Sections[index].Markdown {
			return errors.New("rendered plan changed accepted content")
		}
	}
	set, err := parseTouchset(touchset)
	if err != nil {
		return errors.New("rendered touchset is invalid")
	}
	wantGlobs := manifestGlobs(Manifest{Sections: record.Manifest.Sections[:len(record.Sections)]})
	if !sameStrings(set.Globs, wantGlobs) {
		return errors.New("rendered pair is inconsistent")
	}
	return nil
}

func validateDurablePair(worktree string, record authorityRecord) error {
	expectedPlan, expectedTouchset, err := renderPair(record.Sections)
	if err != nil {
		return authorityError(ErrorAuthorityState, "", "durable planner state is invalid", err)
	}
	actualPlan, err := os.ReadFile(filepath.Join(worktree, "plan.md"))
	if err != nil || !bytes.Equal(actualPlan, expectedPlan) {
		return authorityError(ErrorAuthorityState, "", "planner content does not match durable state", nil)
	}
	actualTouchset, err := os.ReadFile(filepath.Join(worktree, "touchset.json"))
	if err != nil || !bytes.Equal(actualTouchset, expectedTouchset) {
		return authorityError(ErrorAuthorityState, "", "touchset does not match durable state", nil)
	}
	return nil
}

// ValidateComplete applies the existing complete-pair gate to the durable
// manifest associated with this authority.
func (a *Authority) ValidateComplete() error {
	release := acquireBindingLock(a.binding)
	defer release()
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
	if err := validateDurableRecord(record); err != nil {
		return finalValidationError("pair", "", "durable planner state is invalid")
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
	release := acquireBindingLock(a.binding)
	defer release()
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
		return nil, authorityError(ErrorAuthorityState, "", "durable authority record is invalid", err)
	}
	if err := validateDurableRecord(record); err != nil {
		return nil, authorityError(ErrorAuthorityState, "", "durable authority record is invalid", err)
	}
	if err := validateDurablePair(a.binding.Worktree, record); err != nil {
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
	return authorityError(ErrorDescriptor, "", "descriptor is not authoritative", nil)
}

// Expire closes the authority and retains the historical registry state.
func (a *Authority) Expire() error {
	release := acquireBindingLock(a.binding)
	defer release()
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return nil
	}
	if err := a.verifyLocked(); err != nil {
		return err
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
	release := acquireBindingLock(a.binding)
	defer release()
	a.mu.Lock()
	defer a.mu.Unlock()
	a.closed = true
	return nil
}
