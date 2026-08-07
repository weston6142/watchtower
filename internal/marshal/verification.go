package marshal

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strings"

	"github.com/weston6142/watchtower/internal/verificationcache"
)

type Verification struct {
	BaseSHA       string         `json:"base_sha"`
	BranchSHA     string         `json:"branch_sha"`
	TreeSHA       string         `json:"tree_sha"`
	Passed        bool           `json:"passed"`
	Commands      [][]string     `json:"commands"`
	CacheEvidence *CacheEvidence `json:"cache_evidence,omitempty"`
}

// CacheEvidence binds a receipt to the exact lease and verification command
// that produced it. It is absent for non-cache-managed verification paths.
type CacheEvidence struct {
	LeaseID       string                                    `json:"lease_id"`
	State         string                                    `json:"state"`
	Repository    string                                    `json:"repository"`
	ManagedScope  string                                    `json:"managed_scope"`
	BaseSHA       string                                    `json:"base_sha"`
	BranchSHA     string                                    `json:"branch_sha"`
	TreeSHA       string                                    `json:"tree_sha"`
	CommandDigest string                                    `json:"command_argv_digest"`
	SeedLeaseID   string                                    `json:"seed_lease_id"`
	Quarantines   []verificationcache.QuarantineDisposition `json:"quarantines,omitempty"`
}

// CacheIdentity is the current identity a cache-managed receipt must match.
type CacheIdentity struct {
	LeaseID       string
	Repository    string
	ManagedScope  string
	BaseSHA       string
	BranchSHA     string
	TreeSHA       string
	CommandDigest string
}

func (e CacheEvidence) Validate() error {
	if strings.TrimSpace(e.LeaseID) == "" || strings.TrimSpace(e.Repository) == "" ||
		strings.TrimSpace(e.ManagedScope) == "" || strings.TrimSpace(e.BaseSHA) == "" ||
		strings.TrimSpace(e.BranchSHA) == "" || strings.TrimSpace(e.TreeSHA) == "" ||
		strings.TrimSpace(e.SeedLeaseID) == "" {
		return fmt.Errorf("cache evidence identity and scope are required")
	}
	if e.State != string(verificationcache.StateComplete) {
		return fmt.Errorf("cache evidence state %q is not complete", e.State)
	}
	if len(e.CommandDigest) != 64 {
		return fmt.Errorf("cache evidence command digest must be SHA-256 hex")
	}
	if _, err := hex.DecodeString(e.CommandDigest); err != nil {
		return fmt.Errorf("cache evidence command digest: %w", err)
	}
	return nil
}

func (e CacheEvidence) ValidateAgainst(current CacheIdentity) error {
	if err := e.Validate(); err != nil {
		return err
	}
	checks := []struct {
		name string
		got  string
		want string
	}{
		{"repository", e.Repository, current.Repository},
		{"base SHA", e.BaseSHA, current.BaseSHA},
		{"branch SHA", e.BranchSHA, current.BranchSHA},
		{"tree SHA", e.TreeSHA, current.TreeSHA},
		{"lease ID", e.LeaseID, current.LeaseID},
		{"managed scope", e.ManagedScope, current.ManagedScope},
		{"command argv digest", e.CommandDigest, current.CommandDigest},
	}
	for _, check := range checks {
		if check.got != check.want {
			return fmt.Errorf("cache evidence %s %q does not match current %q", check.name, check.got, check.want)
		}
	}
	return nil
}

func LoadVerification(path string) (Verification, error) {
	file, err := os.Open(path)
	if err != nil {
		return Verification{}, err
	}
	defer file.Close()
	decoder := json.NewDecoder(file)
	decoder.DisallowUnknownFields()
	var verification Verification
	if err := decoder.Decode(&verification); err != nil {
		return Verification{}, fmt.Errorf("decode verification: %w", err)
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return Verification{}, err
	}
	if err := verification.Validate(); err != nil {
		return Verification{}, err
	}
	return verification, nil
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return fmt.Errorf("decode verification: trailing JSON value")
		}
		return fmt.Errorf("decode verification: %w", err)
	}
	return nil
}

func (v Verification) Validate() error {
	if strings.TrimSpace(v.BaseSHA) == "" ||
		strings.TrimSpace(v.BranchSHA) == "" ||
		strings.TrimSpace(v.TreeSHA) == "" {
		return fmt.Errorf("verification commit and tree SHAs are required")
	}
	if !v.Passed {
		return fmt.Errorf("verification did not pass")
	}
	if err := validateCommands(v.Commands); err != nil {
		return err
	}
	if v.CacheEvidence != nil {
		return v.CacheEvidence.Validate()
	}
	return nil
}

func validateCommands(commands [][]string) error {
	if len(commands) == 0 {
		return fmt.Errorf("verification commands are required")
	}
	for commandIndex, argv := range commands {
		if len(argv) == 0 {
			return fmt.Errorf("verification command %d has empty argv", commandIndex)
		}
		for argumentIndex, argument := range argv {
			if strings.TrimSpace(argument) == "" {
				return fmt.Errorf(
					"verification command %d argument %d is empty",
					commandIndex, argumentIndex)
			}
		}
	}
	return nil
}

func (v Verification) AppliesTo(treeSHA string) bool {
	return v.Passed && v.TreeSHA != "" && v.TreeSHA == treeSHA
}

func (v Verification) Includes(required []string) bool {
	if len(required) == 0 {
		return true
	}
	for _, command := range v.Commands {
		if slices.Equal(command, required) {
			return true
		}
	}
	return false
}

func Replay(ctx context.Context, dir string, commands [][]string) error {
	return ReplayWithEnvironment(ctx, dir, commands, nil)
}

// ReplayWithEnvironment executes exact argv with a replacement overlay for
// managed environment keys while preserving unrelated inherited values.
func ReplayWithEnvironment(ctx context.Context, dir string, commands [][]string, overlay []string) error {
	if err := validateCommands(commands); err != nil {
		return err
	}
	for _, argv := range commands {
		command := exec.CommandContext(ctx, argv[0], argv[1:]...)
		command.Dir = dir
		command.Env = mergeEnvironment(os.Environ(), overlay)
		output, err := command.CombinedOutput()
		if err != nil {
			return fmt.Errorf(
				"verification command %q failed: %v: %s",
				strings.Join(argv, " "), err, truncate(string(output), maxTestOutputBytes))
		}
	}
	return nil
}

func mergeEnvironment(inherited, overlay []string) []string {
	managed := make(map[string]string, len(overlay))
	for _, entry := range overlay {
		key, value, ok := strings.Cut(entry, "=")
		if ok && key != "" {
			managed[key] = value
		}
	}
	result := make([]string, 0, len(inherited)+len(managed))
	for _, entry := range inherited {
		key, _, ok := strings.Cut(entry, "=")
		if ok {
			if _, replace := managed[key]; replace {
				continue
			}
		}
		result = append(result, entry)
	}
	for _, entry := range overlay {
		key, _, ok := strings.Cut(entry, "=")
		if ok && key != "" {
			if managed[key] == entry[len(key)+1:] {
				result = append(result, entry)
				delete(managed, key)
			}
		}
	}
	return result
}
