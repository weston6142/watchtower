package repocfg

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// Config mirrors the daemon flags. Zero fields are filled from Default()
// after unmarshalling, so a partial config.yaml is fine.
type Config struct {
	Flows        string  `yaml:"flows"`
	Packages     string  `yaml:"packages"`
	Runner       string  `yaml:"runner"`
	Slots        int     `yaml:"slots"`
	Budget       int     `yaml:"budget"`
	PricePerMTok float64 `yaml:"price_per_mtok"`
	ClaudeBin    string  `yaml:"claude_bin"`
	TestCmd      string  `yaml:"test_cmd"`
	Theme        string  `yaml:"theme"`
}

func Default() Config {
	return Config{
		Flows:     filepath.Join(".guildhall", "flows"),
		Packages:  filepath.Join(".guildhall", "packages"),
		Runner:    "claude",
		Slots:     4,
		ClaudeBin: "claude",
	}
}

// ConfigPath returns the config file location under repoRoot.
func ConfigPath(repoRoot string) string {
	return filepath.Join(repoRoot, ".guildhall", "config.yaml")
}

// Load reads .guildhall/config.yaml under repoRoot. A missing file yields
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
	return cfg, nil
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
}

// FindRepo walks up from startDir to the first directory containing .guildhall/.
func FindRepo(startDir string) (string, error) {
	dir, err := filepath.Abs(startDir)
	if err != nil {
		return "", err
	}
	for {
		if fi, err := os.Stat(filepath.Join(dir, ".guildhall")); err == nil && fi.IsDir() {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("no .guildhall found above %s (run 'guildhall init' in your repo)", startDir)
		}
		dir = parent
	}
}

// repoIDLen is the number of hex chars kept from the path hash — short
// enough for socket paths, long enough to avoid collisions in practice.
const repoIDLen = 12

// RepoID is a short stable identifier for a repo path.
func RepoID(repoRoot string) string {
	abs, err := filepath.Abs(repoRoot)
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
