package retry

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/weston6142/watchtower/internal/failure"
)

type dimensionIdentity struct {
	Identity string `json:"identity"`
	Digest   string `json:"digest"`
}

func digestCanonical(value any) string {
	raw, err := json.Marshal(value)
	if err != nil {
		return failure.Unavailable
	}
	digest := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(digest[:])
}

func BuildContextKey(issueID, stage string, site failure.Site, class failure.Class, fingerprint string) string {
	return digestCanonical(struct {
		IssueID      string        `json:"issue_id"`
		Stage        string        `json:"stage"`
		FailureSite  failure.Site  `json:"failure_site"`
		FailureClass failure.Class `json:"failure_class"`
		Fingerprint  string        `json:"fingerprint"`
	}{issueID, stage, site, class, fingerprint})
}

func canonicalContents(values []failure.ContentIdentity) []dimensionIdentity {
	identities := make([]dimensionIdentity, 0, len(values))
	for _, value := range values {
		identities = append(identities, dimensionIdentity{
			Identity: value.Identity,
			Digest:   firstDimensionValue(value.SHA256, value.ContentHash),
		})
	}
	sort.Slice(identities, func(i, j int) bool {
		if identities[i].Identity == identities[j].Identity {
			return identities[i].Digest < identities[j].Digest
		}
		return identities[i].Identity < identities[j].Identity
	})
	return identities
}

func canonicalInputs(values []failure.InputIdentity) []dimensionIdentity {
	identities := make([]dimensionIdentity, 0, len(values))
	for _, value := range values {
		identities = append(identities, dimensionIdentity{
			Identity: value.Identity,
			Digest:   firstDimensionValue(value.SHA256, value.Digest),
		})
	}
	sort.Slice(identities, func(i, j int) bool {
		if identities[i].Identity == identities[j].Identity {
			return identities[i].Digest < identities[j].Digest
		}
		return identities[i].Identity < identities[j].Identity
	})
	return identities
}

func firstDimensionValue(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return failure.Unavailable
}

// BuildStateVector converts transient sanitized identities into four stable,
// independently comparable digests. A missing authoritative tree,
// configuration, or environment identity remains unavailable and cannot be
// mistaken for a state change. An empty decision set is a stable decision
// state and receives its own digest.
func BuildStateVector(inputs failure.FingerprintInputs) StateVector {
	treeIdentity := firstDimensionValue(inputs.Git.TreeIdentity, inputs.Git.Tree)
	branchIdentity := firstDimensionValue(inputs.Git.BranchCommit, inputs.Git.Branch)
	tree := failure.Unavailable
	if treeIdentity != failure.Unavailable && branchIdentity != failure.Unavailable {
		tree = digestCanonical(struct {
			Repository string `json:"repository"`
			Base       string `json:"base"`
			Branch     string `json:"branch"`
			Tree       string `json:"tree"`
		}{
			Repository: firstDimensionValue(inputs.Git.Repository),
			Base:       firstDimensionValue(inputs.Git.BaseCommit, inputs.Git.Base),
			Branch:     branchIdentity,
			Tree:       treeIdentity,
		})
	}

	configurationIdentity := firstDimensionValue(inputs.ConfigurationIdentity, inputs.ConfigIdentity)
	configuration := failure.Unavailable
	if configurationIdentity != failure.Unavailable {
		configuration = digestCanonical(struct {
			Stage         string              `json:"stage"`
			Configuration string              `json:"configuration"`
			StageInputs   []dimensionIdentity `json:"stage_inputs"`
			Verification  string              `json:"verification"`
		}{
			Stage: inputs.Stage, Configuration: configurationIdentity,
			StageInputs:  canonicalInputs(firstInputs(inputs.StageInputs, inputs.StageInputIdentities)),
			Verification: firstDimensionValue(inputs.Verification.CommandIdentity, inputs.Verification.Command),
		})
	}

	environment := failure.Unavailable
	if inputs.EnvironmentIdentity != "" && inputs.EnvironmentIdentity != failure.Unavailable {
		environment = digestCanonical(struct {
			Environment string `json:"environment"`
			Runtime     string `json:"runtime"`
			Cache       string `json:"cache"`
		}{
			Environment: inputs.EnvironmentIdentity,
			Runtime:     firstDimensionValue(inputs.WatchtowerIdentity),
			Cache:       firstDimensionValue(inputs.Verification.CacheIdentity, inputs.Verification.Cache),
		})
	}

	decision := digestCanonical(canonicalContents(firstContents(inputs.Decisions, inputs.DecisionIdentities)))
	return StateVector{TreeDigest: tree, ConfigDigest: configuration, EnvironmentDigest: environment, DecisionDigest: decision}
}

func firstInputs(primary, alias []failure.InputIdentity) []failure.InputIdentity {
	if primary != nil {
		return primary
	}
	return alias
}

func firstContents(primary, alias []failure.ContentIdentity) []failure.ContentIdentity {
	if primary != nil {
		return primary
	}
	return alias
}

func UnavailableDimensions(vector StateVector) []string {
	var unavailable []string
	for _, dimension := range []struct {
		name  string
		value string
	}{
		{"tree", vector.TreeDigest}, {"config", vector.ConfigDigest},
		{"environment", vector.EnvironmentDigest}, {"decision", vector.DecisionDigest},
	} {
		if dimension.value == failure.Unavailable || strings.TrimSpace(dimension.value) == "" {
			unavailable = append(unavailable, dimension.name)
		}
	}
	return unavailable
}

func ChangedDimensions(previous, current StateVector) (changed, unchanged []string) {
	for _, dimension := range []struct {
		name              string
		previous, current string
	}{
		{"tree", previous.TreeDigest, current.TreeDigest},
		{"config", previous.ConfigDigest, current.ConfigDigest},
		{"environment", previous.EnvironmentDigest, current.EnvironmentDigest},
		{"decision", previous.DecisionDigest, current.DecisionDigest},
	} {
		if dimension.previous != dimension.current {
			changed = append(changed, dimension.name)
		} else {
			unchanged = append(unchanged, dimension.name)
		}
	}
	return changed, unchanged
}

func availabilityError(vector StateVector) error {
	if dimensions := UnavailableDimensions(vector); len(dimensions) > 0 {
		return fmt.Errorf("state fingerprint unavailable: %s", strings.Join(dimensions, ","))
	}
	return failure.ValidateStateVector(vector)
}
