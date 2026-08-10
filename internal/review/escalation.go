package review

import (
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	pathpkg "path"
	"path/filepath"
	"sort"
	"strings"
)

// ApprovalFloor is the minimum authority required for an actionable review.
// Floors are deliberately ordered so policy composition is monotonic.
type ApprovalFloor uint8

const (
	FloorNone ApprovalFloor = iota
	FloorPolicy
	FloorOperator
)

func (f ApprovalFloor) valid() bool { return f <= FloorOperator }

func (f ApprovalFloor) String() string {
	switch f {
	case FloorNone:
		return "none"
	case FloorPolicy:
		return "policy"
	case FloorOperator:
		return "operator"
	default:
		return "unknown"
	}
}

const (
	OutcomeApproved         Outcome = "approved"
	OutcomeRequiresApproval Outcome = "requires-approval"
	OutcomeInvalidContext   Outcome = "invalid-context"
	OutcomePolicyError      Outcome = "policy-error"
)

type ItemKind string

const (
	ItemArtifact ItemKind = "artifact"
	ItemRepair   ItemKind = "repair"
	ItemDecision ItemKind = "decision"
)

type ItemBinding struct {
	Kind      ItemKind `json:"kind"`
	Hash      string   `json:"hash"`
	Path      string   `json:"path"`
	Operation string   `json:"operation"`
}

type DependencyBinding struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
	Hash string `json:"hash"`
}

type PathFloor struct {
	Glob  string        `json:"glob"`
	Floor ApprovalFloor `json:"floor"`
}

type Evidence struct {
	Signal string        `json:"signal"`
	Value  string        `json:"value"`
	Rule   string        `json:"rule"`
	Floor  ApprovalFloor `json:"floor"`
}

type ModelMetadata struct {
	Importance *float64 `json:"importance,omitempty"`
	Options    []string `json:"options,omitempty"`
	Rationale  string   `json:"rationale,omitempty"`
}

type Policy struct {
	ID               string                   `json:"policy_id"`
	Version          string                   `json:"policy_version"`
	Valid            bool                     `json:"valid"`
	DefaultFloor     ApprovalFloor            `json:"default_floor"`
	StageFloors      map[string]ApprovalFloor `json:"stage_floors,omitempty"`
	OperationFloors  map[string]ApprovalFloor `json:"operation_floors,omitempty"`
	PathFloors       []PathFloor              `json:"path_floors,omitempty"`
	DestructiveFloor ApprovalFloor            `json:"destructive_floor"`
	PublicationFloor ApprovalFloor            `json:"publication_floor"`
}

type EscalationPolicy = Policy

type EscalationContext struct {
	Stage     string
	Operation string
	Paths     []string
	// RiskFactsValid distinguishes an explicitly evaluated low-risk decision
	// from a caller that omitted the destructive/publication facts entirely.
	RiskFactsValid  bool
	DestructiveRisk bool
	PublicationRisk bool
	Policy          Policy
	Item            ItemBinding
	Dependencies    []DependencyBinding
	Model           ModelMetadata
}

type Evaluation struct {
	Outcome        Outcome             `json:"outcome"`
	RequiredFloor  ApprovalFloor       `json:"required_floor"`
	EffectiveFloor ApprovalFloor       `json:"effective_floor"`
	PolicyID       string              `json:"policy_id"`
	PolicyVersion  string              `json:"policy_version"`
	Evidence       []Evidence          `json:"evidence,omitempty"`
	Item           ItemBinding         `json:"item"`
	Dependencies   []DependencyBinding `json:"dependencies,omitempty"`
	Model          ModelMetadata       `json:"model"`
	Err            error               `json:"-"`
}

// ValidatePolicy checks policy identity and every configured floor before any
// risk signal is evaluated. Invalid policy state is never treated as a safe
// default.
func ValidatePolicy(policy Policy) error {
	if !policy.Valid {
		return errors.New("decision escalation policy is invalid")
	}
	if strings.TrimSpace(policy.ID) == "" || strings.TrimSpace(policy.Version) == "" {
		return errors.New("decision escalation policy identity is incomplete")
	}
	if !policy.DefaultFloor.valid() || !policy.DestructiveFloor.valid() || !policy.PublicationFloor.valid() {
		return errors.New("decision escalation policy contains an unknown floor")
	}
	if err := validateFloorMap(policy.StageFloors, "stage"); err != nil {
		return err
	}
	if err := validateFloorMap(policy.OperationFloors, "operation"); err != nil {
		return err
	}
	seen := make(map[string]struct{}, len(policy.PathFloors))
	for _, rule := range policy.PathFloors {
		glob, err := canonicalGlob(rule.Glob)
		if err != nil {
			return err
		}
		if _, err := pathpkg.Match(glob, ""); err != nil {
			return fmt.Errorf("path rule %q has malformed glob: %w", glob, err)
		}
		if !rule.Floor.valid() {
			return fmt.Errorf("path rule %q has unknown floor", glob)
		}
		if _, exists := seen[glob]; exists {
			return fmt.Errorf("duplicate path rule %q", glob)
		}
		seen[glob] = struct{}{}
	}
	return nil
}

func validateFloorMap(rules map[string]ApprovalFloor, kind string) error {
	for key, floor := range rules {
		if _, err := canonicalToken(key, kind); err != nil {
			return err
		}
		if !floor.valid() {
			return fmt.Errorf("%s rule %q has unknown floor", kind, key)
		}
	}
	return nil
}

// ValidateContext validates all safety inputs and exact item identities. It
// intentionally rejects non-canonical values instead of silently rewriting an
// approval target.
func ValidateContext(ctx EscalationContext) error {
	stage, err := canonicalToken(ctx.Stage, "stage")
	if err != nil || stage != ctx.Stage {
		return fmt.Errorf("invalid stage: %q", ctx.Stage)
	}
	operation, err := canonicalToken(ctx.Operation, "operation")
	if err != nil || operation != ctx.Operation {
		return fmt.Errorf("invalid operation: %q", ctx.Operation)
	}
	if len(ctx.Paths) == 0 {
		return errors.New("affected paths are empty")
	}
	if !ctx.RiskFactsValid {
		return errors.New("destructive and publication risk facts are missing")
	}
	seenPaths := make(map[string]struct{}, len(ctx.Paths))
	for _, raw := range ctx.Paths {
		canonical, pathErr := canonicalPath(raw)
		if pathErr != nil || canonical != raw {
			return fmt.Errorf("invalid affected path: %q", raw)
		}
		if _, exists := seenPaths[canonical]; exists {
			return fmt.Errorf("duplicate affected path: %q", canonical)
		}
		seenPaths[canonical] = struct{}{}
	}
	if err := validateItem(ctx.Item); err != nil {
		return err
	}
	if ctx.Item.Operation != operation {
		return fmt.Errorf("item operation %q does not match context operation %q", ctx.Item.Operation, operation)
	}
	if _, exists := seenPaths[ctx.Item.Path]; !exists {
		return fmt.Errorf("item path %q is not affected", ctx.Item.Path)
	}
	seenDependencies := make(map[string]struct{}, len(ctx.Dependencies))
	for _, dependency := range ctx.Dependencies {
		if strings.TrimSpace(dependency.Kind) == "" || strings.TrimSpace(dependency.ID) == "" {
			return errors.New("dependency identity is incomplete")
		}
		if err := validateHash(dependency.Hash); err != nil {
			return fmt.Errorf("invalid dependency %s/%s: %w", dependency.Kind, dependency.ID, err)
		}
		key := dependency.Kind + "\x00" + dependency.ID
		if _, exists := seenDependencies[key]; exists {
			return fmt.Errorf("duplicate dependency %s/%s", dependency.Kind, dependency.ID)
		}
		seenDependencies[key] = struct{}{}
	}
	return nil
}

func validateItem(item ItemBinding) error {
	switch item.Kind {
	case ItemArtifact, ItemRepair, ItemDecision:
	default:
		return fmt.Errorf("unknown item kind %q", item.Kind)
	}
	canonical, err := canonicalPath(item.Path)
	if err != nil || canonical != item.Path {
		return fmt.Errorf("invalid item path: %q", item.Path)
	}
	operation, err := canonicalToken(item.Operation, "item operation")
	if err != nil || operation != item.Operation {
		return fmt.Errorf("invalid item operation: %q", item.Operation)
	}
	if err := validateHash(item.Hash); err != nil {
		return fmt.Errorf("invalid item hash: %w", err)
	}
	return nil
}

func validateHash(hash string) error {
	if len(hash) != 64 {
		return errors.New("sha256 must contain 64 hexadecimal characters")
	}
	if _, err := hex.DecodeString(hash); err != nil {
		return fmt.Errorf("sha256 is not hexadecimal: %w", err)
	}
	if strings.ToLower(hash) != hash {
		return errors.New("sha256 must be lowercase")
	}
	return nil
}

func canonicalToken(raw, kind string) (string, error) {
	value := strings.TrimSpace(raw)
	if value == "" || value != raw || strings.ContainsAny(value, "/\\") {
		return "", fmt.Errorf("%s is not canonical: %q", kind, raw)
	}
	for _, r := range value {
		if r < 'a' || r > 'z' {
			if (r < '0' || r > '9') && r != '-' && r != '_' && r != '.' {
				return "", fmt.Errorf("%s contains unsupported character: %q", kind, raw)
			}
		}
	}
	return value, nil
}

func canonicalPath(raw string) (string, error) {
	if raw == "" {
		return "", errors.New("path is empty")
	}
	native := filepath.FromSlash(raw)
	if filepath.IsAbs(native) {
		return "", errors.New("path is absolute")
	}
	clean := filepath.ToSlash(filepath.Clean(native))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || strings.Contains(clean, "\\") {
		return "", errors.New("path escapes repository")
	}
	return clean, nil
}

func canonicalGlob(raw string) (string, error) {
	value := strings.TrimSpace(raw)
	if value == "" || filepath.IsAbs(filepath.FromSlash(value)) || strings.HasPrefix(value, "../") || value == ".." {
		return "", fmt.Errorf("invalid path glob %q", raw)
	}
	if filepath.ToSlash(filepath.Clean(filepath.FromSlash(value))) != value {
		return "", fmt.Errorf("path glob is not canonical: %q", raw)
	}
	return value, nil
}

// Evaluate computes the engine-owned floor. The returned result is never an
// approval for a non-none floor; the centralized gate adds that authority.
func Evaluate(ctx EscalationContext) Evaluation {
	result := Evaluation{
		Item: ctx.Item, Dependencies: append([]DependencyBinding(nil), ctx.Dependencies...), Model: ctx.Model,
		PolicyID: ctx.Policy.ID, PolicyVersion: ctx.Policy.Version,
	}
	if err := ValidateContext(ctx); err != nil {
		result.Outcome, result.Err = OutcomeInvalidContext, err
		return result
	}
	if err := ValidatePolicy(ctx.Policy); err != nil {
		result.Outcome, result.Err = OutcomePolicyError, err
		return result
	}
	result.Evidence = evaluateEvidence(ctx)
	for _, evidence := range result.Evidence {
		if evidence.Floor > result.RequiredFloor {
			result.RequiredFloor = evidence.Floor
		}
	}
	result.EffectiveFloor = result.RequiredFloor
	if requested := modelFloor(ctx.Model); requested > result.EffectiveFloor {
		result.EffectiveFloor = requested
	}
	if result.EffectiveFloor == FloorNone {
		result.Outcome = OutcomeApproved
	} else {
		result.Outcome = OutcomeRequiresApproval
	}
	sort.Slice(result.Dependencies, func(i, j int) bool {
		if result.Dependencies[i].Kind == result.Dependencies[j].Kind {
			return result.Dependencies[i].ID < result.Dependencies[j].ID
		}
		return result.Dependencies[i].Kind < result.Dependencies[j].Kind
	})
	return result
}

func evaluateEvidence(ctx EscalationContext) []Evidence {
	evidence := make([]Evidence, 0, 8+len(ctx.Paths))
	add := func(signal, value, rule string, floor ApprovalFloor) {
		evidence = append(evidence, Evidence{Signal: signal, Value: value, Rule: rule, Floor: floor})
	}
	stageFloor := ctx.Policy.StageFloors[ctx.Stage]
	add("stage", ctx.Stage, "stage:"+ctx.Stage, stageFloor)
	operationFloor := ctx.Policy.OperationFloors[ctx.Operation]
	add("operation", ctx.Operation, "operation:"+ctx.Operation, operationFloor)
	for _, affectedPath := range ctx.Paths {
		for _, pathRule := range ctx.Policy.PathFloors {
			if matchesGlob(pathRule.Glob, affectedPath) {
				add("path", affectedPath, "path:"+pathRule.Glob, pathRule.Floor)
			}
		}
	}
	if ctx.DestructiveRisk {
		add("destructive", "true", "destructive-risk", ctx.Policy.DestructiveFloor)
	}
	if ctx.PublicationRisk {
		add("publication", "true", "publication-risk", ctx.Policy.PublicationFloor)
	}
	add("policy", ctx.Policy.ID+"@"+ctx.Policy.Version, "default", ctx.Policy.DefaultFloor)
	sort.Slice(evidence, func(i, j int) bool { return evidenceLess(evidence[i], evidence[j]) })
	return evidence
}

const (
	modelImportanceNoRequest         = 0.0
	modelImportancePolicyThreshold   = 0.5
	modelImportanceOperatorThreshold = 1.0
)

func modelFloor(model ModelMetadata) ApprovalFloor {
	if model.Importance == nil || math.IsNaN(*model.Importance) || math.IsInf(*model.Importance, 0) ||
		*model.Importance <= modelImportanceNoRequest || *model.Importance > modelImportanceOperatorThreshold {
		return FloorNone
	}
	if *model.Importance >= modelImportanceOperatorThreshold {
		return FloorOperator
	}
	if *model.Importance >= modelImportancePolicyThreshold {
		return FloorPolicy
	}
	return FloorNone
}

func evidenceLess(left, right Evidence) bool {
	if left.Signal != right.Signal {
		return left.Signal < right.Signal
	}
	if left.Value != right.Value {
		return left.Value < right.Value
	}
	if left.Rule != right.Rule {
		return left.Rule < right.Rule
	}
	return left.Floor < right.Floor
}

func matchesGlob(pattern, candidate string) bool {
	if strings.HasSuffix(pattern, "/**") {
		return strings.HasPrefix(candidate, strings.TrimSuffix(pattern, "**"))
	}
	matched, err := pathpkg.Match(pattern, candidate)
	return err == nil && matched
}
