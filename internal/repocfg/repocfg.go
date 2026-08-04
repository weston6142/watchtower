package repocfg

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"unicode"

	"github.com/weston6142/watchtower/internal/review"
	"gopkg.in/yaml.v3"
)

// Config mirrors the daemon flags. Zero fields are filled from Default()
// after unmarshalling, so a partial config.yaml is fine.
type Config struct {
	Flows        string           `yaml:"flows"`
	Packages     string           `yaml:"packages"`
	Runner       string           `yaml:"runner"`
	Slots        int              `yaml:"slots"`
	Budget       int              `yaml:"budget"`
	PricePerMTok float64          `yaml:"price_per_mtok"`
	ClaudeBin    string           `yaml:"claude_bin"`
	CodexBin     string           `yaml:"codex_bin"`
	CodexModel   string           `yaml:"codex_model"`
	CodexEffort  string           `yaml:"codex_effort"`
	TestCmd      string           `yaml:"test_cmd"`
	TestArgv     []string         `yaml:"-"`
	Theme        string           `yaml:"theme"`
	Pull         bool             `yaml:"pull"`
	Push         bool             `yaml:"push"`
	PlanReview   PlanReviewConfig `yaml:"plan_review"`
}

type PlanReviewConfig struct {
	PolicyID           string `yaml:"policy_id"`
	PolicyVersion      string `yaml:"policy_version"`
	AutoApproveRegular bool   `yaml:"auto_approve_regular"`
	Valid              bool   `yaml:"-"`
}

func Default() Config {
	return Config{
		Flows:       filepath.Join(".watchtower", "flows"),
		Packages:    filepath.Join(".watchtower", "packages"),
		Runner:      "codex",
		Slots:       4,
		ClaudeBin:   "claude",
		CodexBin:    "codex",
		CodexModel:  "gpt-5.6-luna",
		CodexEffort: "xhigh",
		// Fast-forwarding the base from origin is safe, so it defaults on;
		// publishing merges is a bigger step, so pushing stays opt-in.
		Pull: true,
		PlanReview: PlanReviewConfig{
			PolicyID: "manual-default", PolicyVersion: "1", Valid: true,
		},
	}
}

func (c *PlanReviewConfig) UnmarshalYAML(node *yaml.Node) error {
	var decoded struct {
		PolicyID           string `yaml:"policy_id"`
		PolicyVersion      string `yaml:"policy_version"`
		AutoApproveRegular bool   `yaml:"auto_approve_regular"`
	}
	if node.Kind != yaml.MappingNode || node.Decode(&decoded) != nil {
		*c = PlanReviewConfig{}
		return nil
	}
	*c = PlanReviewConfig{
		PolicyID:           decoded.PolicyID,
		PolicyVersion:      decoded.PolicyVersion,
		AutoApproveRegular: decoded.AutoApproveRegular,
		Valid:              true,
	}
	return nil
}

func (c Config) PlanReviewSettings() review.PolicySettings {
	return review.PolicySettings{
		ID:                 c.PlanReview.PolicyID,
		Version:            c.PlanReview.PolicyVersion,
		AutoApproveRegular: c.PlanReview.AutoApproveRegular,
		Valid:              c.PlanReview.Valid,
	}
}

// ConfigPath returns the config file location under repoRoot.
func ConfigPath(repoRoot string) string {
	return filepath.Join(repoRoot, ".watchtower", "config.yaml")
}

// Load reads .watchtower/config.yaml under repoRoot. A missing file yields
// defaults. Relative Flows/Packages are resolved against repoRoot.
func Load(repoRoot string) (Config, error) {
	cfg := Default()
	b, err := os.ReadFile(ConfigPath(repoRoot))
	if err == nil {
		if err := yaml.Unmarshal(b, &cfg); err != nil {
			return Config{}, fmt.Errorf("%s: %w", ConfigPath(repoRoot), err)
		}
		fillGaps(&cfg)
	} else if !os.IsNotExist(err) {
		return Config{}, err
	}
	if !filepath.IsAbs(cfg.Flows) {
		cfg.Flows = filepath.Join(repoRoot, cfg.Flows)
	}
	if !filepath.IsAbs(cfg.Packages) {
		cfg.Packages = filepath.Join(repoRoot, cfg.Packages)
	}
	cfg.TestArgv, err = ParseCommand(cfg.TestCmd)
	if err != nil {
		return Config{}, fmt.Errorf("%s test_cmd: %w", ConfigPath(repoRoot), err)
	}
	return cfg, nil
}

// ParseCommand converts a configured command line to argv once, honoring
// quoting without ever invoking a shell.
func ParseCommand(command string) ([]string, error) {
	var argv []string
	var current strings.Builder
	var quote rune
	escaped := false
	started := false
	flush := func() error {
		if !started {
			return nil
		}
		value := current.String()
		if value == "" {
			return fmt.Errorf("empty command argument")
		}
		argv = append(argv, value)
		current.Reset()
		started = false
		return nil
	}
	for _, char := range command {
		if escaped {
			current.WriteRune(char)
			started = true
			escaped = false
			continue
		}
		if char == '\\' && quote != '\'' {
			escaped = true
			started = true
			continue
		}
		if quote != 0 {
			if char == quote {
				quote = 0
			} else {
				current.WriteRune(char)
			}
			started = true
			continue
		}
		if char == '\'' || char == '"' {
			quote = char
			started = true
			continue
		}
		if unicode.IsSpace(char) {
			if err := flush(); err != nil {
				return nil, err
			}
			continue
		}
		current.WriteRune(char)
		started = true
	}
	if escaped {
		return nil, fmt.Errorf("trailing escape")
	}
	if quote != 0 {
		return nil, fmt.Errorf("unterminated quote")
	}
	if err := flush(); err != nil {
		return nil, err
	}
	return argv, nil
}

func fillGaps(cfg *Config) {
	d := Default()
	if cfg.Flows == "" {
		cfg.Flows = d.Flows
	}
	if cfg.Packages == "" {
		cfg.Packages = d.Packages
	}
	if cfg.Runner == "" {
		cfg.Runner = d.Runner
	}
	if cfg.Slots == 0 {
		cfg.Slots = d.Slots
	}
	if cfg.ClaudeBin == "" {
		cfg.ClaudeBin = d.ClaudeBin
	}
	if cfg.CodexBin == "" {
		cfg.CodexBin = d.CodexBin
	}
	if cfg.CodexModel == "" {
		cfg.CodexModel = d.CodexModel
	}
	if cfg.CodexEffort == "" {
		cfg.CodexEffort = d.CodexEffort
	}
	if cfg.PlanReview.Valid {
		if cfg.PlanReview.PolicyID == "" {
			cfg.PlanReview.PolicyID = d.PlanReview.PolicyID
		}
		if cfg.PlanReview.PolicyVersion == "" {
			cfg.PlanReview.PolicyVersion = d.PlanReview.PolicyVersion
		}
	}
}

// FindRepo resolves linked worktrees to their common checkout, then walks up
// from ordinary checkouts to the first directory containing .watchtower/.
func FindRepo(startDir string) (string, error) {
	start, err := filepath.Abs(startDir)
	if err != nil {
		return "", err
	}
	if repo, ok := boundRepository(start); ok {
		return repo, nil
	}
	gitDir, gitDirErr := gitDirectory(start, "--git-dir")
	commonDir, commonDirErr := gitDirectory(start, "--git-common-dir")
	if gitDirErr == nil && commonDirErr == nil && filepath.Clean(gitDir) != filepath.Clean(commonDir) {
		candidate := filepath.Dir(commonDir)
		if info, statErr := os.Stat(filepath.Join(candidate, ".watchtower")); statErr == nil && info.IsDir() {
			return candidate, nil
		}
	}
	if repo, ok := findWatchtowerParent(start); ok {
		return repo, nil
	}
	if commonDirErr == nil {
		candidate := filepath.Dir(commonDir)
		if info, statErr := os.Stat(filepath.Join(candidate, ".watchtower")); statErr == nil && info.IsDir() {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("no .watchtower found above %s (run 'watchtower init' in your repo)", startDir)
}

const repositoryMarker = "watchtower-repository"

// BindWorktree records the initialized repository that owns a claimed
// workspace in Git-private metadata. This also supports providers whose
// workspace is not linked to the repository's common Git directory.
func BindWorktree(repoRoot, worktree string) error {
	repo, err := canonicalPath(repoRoot)
	if err != nil {
		return err
	}
	marker, err := gitPath(worktree, repositoryMarker)
	if err != nil {
		return fmt.Errorf("locate claimed-workspace metadata: %w", err)
	}
	if err := os.WriteFile(marker, []byte(repo+"\n"), 0o644); err != nil {
		return fmt.Errorf("bind claimed workspace: %w", err)
	}
	return nil
}

func boundRepository(start string) (string, bool) {
	marker, err := gitPath(start, repositoryMarker)
	if err != nil {
		return "", false
	}
	body, err := os.ReadFile(marker)
	if err != nil {
		return "", false
	}
	repo, err := canonicalPath(strings.TrimSpace(string(body)))
	if err != nil {
		return "", false
	}
	if info, err := os.Stat(filepath.Join(repo, ".watchtower")); err == nil && info.IsDir() {
		return repo, true
	}
	return "", false
}

func gitPath(start, name string) (string, error) {
	cmd := exec.Command(
		"git", "-C", start, "rev-parse", "--path-format=absolute", "--git-path", name,
	)
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func canonicalPath(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err == nil {
		return resolved, nil
	}
	return abs, nil
}

func gitDirectory(start, flag string) (string, error) {
	cmd := exec.Command("git", "-C", start, "rev-parse", "--path-format=absolute", flag)
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func findWatchtowerParent(start string) (string, bool) {
	dir := start
	for {
		if fi, err := os.Stat(filepath.Join(dir, ".watchtower")); err == nil && fi.IsDir() {
			return dir, true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", false
		}
		dir = parent
	}
}

// repoIDLen is the number of hex chars kept from the path hash — short
// enough for socket paths, long enough to avoid collisions in practice.
const repoIDLen = 12

// RepoID is a short stable identifier for a repo path.
func RepoID(repoRoot string) string {
	abs, err := canonicalPath(repoRoot)
	if err != nil {
		abs = repoRoot
	}
	sum := sha256.Sum256([]byte(abs))
	return hex.EncodeToString(sum[:])[:repoIDLen]
}

// RepoDataDir is where a repo's db, socket, log, and pidfile live.
func RepoDataDir(base, repoRoot string) string {
	return filepath.Join(base, "repos", RepoID(repoRoot))
}
