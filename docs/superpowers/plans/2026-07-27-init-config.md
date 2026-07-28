# watchtower init + config file Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add `watchtower init`, a per-repo `.watchtower/config.yaml`, per-repo data dirs (fixing the socket collision), a global repo registry with `watchtower repos`, and client auto-spawn of the daemon.

**Architecture:** A new `internal/repocfg` package owns config loading, repo discovery (CWD walk), repo IDs, per-repo data paths, and the registry. A new `internal/scaffold` package embeds default flows/packages and writes them on `init`. `cmd/watchtower/main.go` gains `init` and `repos` commands; `runDaemon` resolves its settings from config with flag override; client dialing gains stale-socket cleanup and detached daemon spawn.

**Tech Stack:** Go, `gopkg.in/yaml.v3` (already a dep), `go:embed`, Unix sockets.

**Spec:** `docs/superpowers/specs/2026-07-27-init-config-design.md`

## Global Constraints

- Config file path: `.watchtower/config.yaml` at the repo root; relative paths inside resolve against the repo root.
- Precedence: explicit CLI flag > config file > built-in default. "Explicit" is determined via `flag.FlagSet.Visit`.
- Built-in defaults: `flows: .watchtower/flows`, `packages: .watchtower/packages`, `runner: claude`, `slots: 4`, `budget: 0`, `price_per_mtok: 0`, `claude_bin: claude`, `test_cmd: ""`.
- Per-repo data dir: `<base>/repos/<id>/` where `<base>` defaults to `~/.local/share/watchtower` (overridable with `--data`) and `<id>` is the first 12 hex chars of sha256 of the repo's absolute path.
- Registry entries: `<base>/repos.d/<id>.yaml` with fields `path` and `registered_at` (RFC3339).
- `init` never overwrites existing files; re-running is a no-op that reports what already exists.
- `WATCHTOWER_FAKE=1` continues to force the fake runner, overriding config.
- All commit messages end with `Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>`.

---

### Task 1: `internal/repocfg` — config load, defaults, repo discovery, repo ID, paths

**Files:**
- Create: `internal/repocfg/repocfg.go`
- Test: `internal/repocfg/repocfg_test.go`

**Interfaces:**
- Produces:
  - `type Config struct { Flows, Packages, Runner string; Slots, Budget int; PricePerMTok float64; ClaudeBin, TestCmd string }` (yaml tags: `flows, packages, runner, slots, budget, price_per_mtok, claude_bin, test_cmd`)
  - `func Default() Config`
  - `func Load(repoRoot string) (Config, error)` — reads `.watchtower/config.yaml` under repoRoot, fills zero fields from `Default()`, resolves `Flows`/`Packages` to absolute paths against repoRoot. Missing file → `Default()` with resolved paths, no error.
  - `func FindRepo(startDir string) (string, error)` — walks up from startDir to the first dir containing `.watchtower/`; error `no .watchtower found (run 'watchtower init' in your repo)` if none.
  - `func RepoID(repoRoot string) string` — 12 hex chars of sha256 of `filepath.Abs(repoRoot)`.
  - `func RepoDataDir(base, repoRoot string) string` — `filepath.Join(base, "repos", RepoID(repoRoot))`.

- [ ] **Step 1: Write the failing tests**

```go
package repocfg

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadMissingFileReturnsDefaults(t *testing.T) {
	root := t.TempDir()
	cfg, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Runner != "claude" || cfg.Slots != 4 || cfg.ClaudeBin != "claude" {
		t.Fatalf("bad defaults: %+v", cfg)
	}
	if cfg.Flows != filepath.Join(root, ".watchtower", "flows") {
		t.Fatalf("flows not resolved: %s", cfg.Flows)
	}
	if cfg.Packages != filepath.Join(root, ".watchtower", "packages") {
		t.Fatalf("packages not resolved: %s", cfg.Packages)
	}
}

func TestLoadReadsFileAndFillsGaps(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, ".watchtower")
	os.MkdirAll(dir, 0o755)
	yaml := "runner: fake\nslots: 2\nflows: myflows\ntest_cmd: \"go test ./...\"\n"
	os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(yaml), 0o644)
	cfg, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Runner != "fake" || cfg.Slots != 2 || cfg.TestCmd != "go test ./..." {
		t.Fatalf("file values not applied: %+v", cfg)
	}
	if cfg.Flows != filepath.Join(root, "myflows") {
		t.Fatalf("relative flows not resolved: %s", cfg.Flows)
	}
	if cfg.ClaudeBin != "claude" { // gap filled from defaults
		t.Fatalf("gap not filled: %+v", cfg)
	}
}

func TestLoadBadYAMLErrors(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, ".watchtower")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("slots: [not an int"), 0o644)
	if _, err := Load(root); err == nil {
		t.Fatal("expected error for bad yaml")
	}
}

func TestFindRepoWalksUp(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, ".watchtower"), 0o755)
	nested := filepath.Join(root, "a", "b")
	os.MkdirAll(nested, 0o755)
	got, err := FindRepo(nested)
	if err != nil {
		t.Fatal(err)
	}
	// t.TempDir may be a symlink on macOS; compare resolved paths.
	wantR, _ := filepath.EvalSymlinks(root)
	gotR, _ := filepath.EvalSymlinks(got)
	if gotR != wantR {
		t.Fatalf("got %s want %s", got, root)
	}
}

func TestFindRepoNotFound(t *testing.T) {
	if _, err := FindRepo(t.TempDir()); err == nil {
		t.Fatal("expected error")
	}
}

func TestRepoIDStableAndShort(t *testing.T) {
	a := RepoID("/some/repo")
	if a != RepoID("/some/repo") || len(a) != 12 {
		t.Fatalf("bad id %q", a)
	}
	if a == RepoID("/other/repo") {
		t.Fatal("ids collide")
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/repocfg/`
Expected: FAIL (package does not compile / functions undefined)

- [ ] **Step 3: Write the implementation**

```go
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
}

func Default() Config {
	return Config{
		Flows:     filepath.Join(".watchtower", "flows"),
		Packages:  filepath.Join(".watchtower", "packages"),
		Runner:    "claude",
		Slots:     4,
		ClaudeBin: "claude",
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

// FindRepo walks up from startDir to the first directory containing .watchtower/.
func FindRepo(startDir string) (string, error) {
	dir, err := filepath.Abs(startDir)
	if err != nil {
		return "", err
	}
	for {
		if fi, err := os.Stat(filepath.Join(dir, ".watchtower")); err == nil && fi.IsDir() {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("no .watchtower found above %s (run 'watchtower init' in your repo)", startDir)
		}
		dir = parent
	}
}

// RepoID is a short stable identifier for a repo path.
func RepoID(repoRoot string) string {
	abs, err := filepath.Abs(repoRoot)
	if err != nil {
		abs = repoRoot
	}
	sum := sha256.Sum256([]byte(abs))
	return hex.EncodeToString(sum[:])[:12]
}

// RepoDataDir is where a repo's db, socket, log, and pidfile live.
func RepoDataDir(base, repoRoot string) string {
	return filepath.Join(base, "repos", RepoID(repoRoot))
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/repocfg/`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/repocfg/
git commit -m "feat: repocfg package — config load, repo discovery, repo IDs

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task 2: Registry — register and list repos

**Files:**
- Create: `internal/repocfg/registry.go`
- Test: `internal/repocfg/registry_test.go`

**Interfaces:**
- Consumes: `RepoID` from Task 1.
- Produces:
  - `type RegistryEntry struct { Path string `yaml:"path"`; RegisteredAt time.Time `yaml:"registered_at"` }`
  - `func Register(base, repoRoot string) error` — writes `<base>/repos.d/<id>.yaml`; overwriting an existing entry for the same repo is fine (idempotent).
  - `func ListRegistered(base string) ([]RegistryEntry, error)` — all entries sorted by Path; missing `repos.d` → empty slice, no error.

- [ ] **Step 1: Write the failing tests**

```go
package repocfg

import (
	"testing"
)

func TestRegisterAndList(t *testing.T) {
	base := t.TempDir()
	if err := Register(base, "/repo/alpha"); err != nil {
		t.Fatal(err)
	}
	if err := Register(base, "/repo/beta"); err != nil {
		t.Fatal(err)
	}
	if err := Register(base, "/repo/alpha"); err != nil { // idempotent
		t.Fatal(err)
	}
	entries, err := ListRegistered(base)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("want 2 entries, got %d", len(entries))
	}
	if entries[0].Path != "/repo/alpha" || entries[1].Path != "/repo/beta" {
		t.Fatalf("bad order: %+v", entries)
	}
	if entries[0].RegisteredAt.IsZero() {
		t.Fatal("registered_at not set")
	}
}

func TestListRegisteredEmpty(t *testing.T) {
	entries, err := ListRegistered(t.TempDir())
	if err != nil || len(entries) != 0 {
		t.Fatalf("want empty no error, got %v %v", entries, err)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/repocfg/ -run 'TestRegister|TestListRegistered'`
Expected: FAIL (undefined: Register)

- [ ] **Step 3: Write the implementation**

```go
package repocfg

import (
	"os"
	"path/filepath"
	"sort"
	"time"

	"gopkg.in/yaml.v3"
)

type RegistryEntry struct {
	Path         string    `yaml:"path"`
	RegisteredAt time.Time `yaml:"registered_at"`
}

func registryDir(base string) string { return filepath.Join(base, "repos.d") }

// Register records repoRoot in the global registry. Re-registering the same
// repo overwrites its entry.
func Register(base, repoRoot string) error {
	if err := os.MkdirAll(registryDir(base), 0o755); err != nil {
		return err
	}
	abs, err := filepath.Abs(repoRoot)
	if err != nil {
		return err
	}
	b, err := yaml.Marshal(RegistryEntry{Path: abs, RegisteredAt: time.Now().UTC()})
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(registryDir(base), RepoID(repoRoot)+".yaml"), b, 0o644)
}

// ListRegistered returns all registered repos sorted by path.
func ListRegistered(base string) ([]RegistryEntry, error) {
	matches, err := filepath.Glob(filepath.Join(registryDir(base), "*.yaml"))
	if err != nil {
		return nil, err
	}
	var out []RegistryEntry
	for _, m := range matches {
		b, err := os.ReadFile(m)
		if err != nil {
			return nil, err
		}
		var e RegistryEntry
		if err := yaml.Unmarshal(b, &e); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}
```

Note on the test's `/repo/alpha` paths: `filepath.Abs` on an already-absolute path returns it unchanged, so no filesystem access is needed — registering a nonexistent path is fine for the unit test.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/repocfg/`
Expected: PASS (all repocfg tests)

- [ ] **Step 5: Commit**

```bash
git add internal/repocfg/registry.go internal/repocfg/registry_test.go
git commit -m "feat: global repo registry with register/list

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task 3: `internal/scaffold` — embedded defaults + Init

**Files:**
- Create: `internal/scaffold/scaffold.go`
- Create: `internal/scaffold/defaults/config.yaml`
- Create: `internal/scaffold/defaults/flows/default.yaml` (copy of `dist/flows/default.yaml`)
- Create: `internal/scaffold/defaults/packages/**` (copy of `dist/packages/**` — each package dir's `package.yaml` + `prompt.md`)
- Test: `internal/scaffold/scaffold_test.go`

**Interfaces:**
- Consumes: nothing from other tasks (pure files).
- Produces: `func Init(repoRoot string) ([]string, []string, error)` — copies the embedded `defaults/` tree into `<repoRoot>/.watchtower/`, returns `(created, skipped)` relative paths. Existing files are never overwritten.

- [ ] **Step 1: Copy default assets into the package**

```bash
mkdir -p internal/scaffold/defaults
cp -R dist/flows internal/scaffold/defaults/flows
cp -R dist/packages internal/scaffold/defaults/packages
```

Then create `internal/scaffold/defaults/config.yaml` with the documented defaults, commented for humans:

```yaml
# watchtower repo configuration. All fields optional; shown values are defaults.
# Relative paths resolve against the repo root.
flows: .watchtower/flows
packages: .watchtower/packages
runner: claude        # claude|fake
slots: 4              # concurrent heavy slots
budget: 0             # per-issue token budget (0=off)
price_per_mtok: 0     # estimated $/Mtok (0=hide cost)
claude_bin: claude
test_cmd: ""          # merge-train test command, e.g. "go test ./..."
```

- [ ] **Step 2: Write the failing test**

```go
package scaffold

import (
	"os"
	"path/filepath"
	"testing"
)

func TestInitCreatesTree(t *testing.T) {
	root := t.TempDir()
	created, skipped, err := Init(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(skipped) != 0 {
		t.Fatalf("fresh init skipped files: %v", skipped)
	}
	if len(created) == 0 {
		t.Fatal("nothing created")
	}
	for _, p := range []string{
		filepath.Join(root, ".watchtower", "config.yaml"),
		filepath.Join(root, ".watchtower", "flows", "default.yaml"),
		filepath.Join(root, ".watchtower", "packages", "executor", "package.yaml"),
		filepath.Join(root, ".watchtower", "packages", "executor", "prompt.md"),
	} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("missing %s: %v", p, err)
		}
	}
}

func TestInitIdempotentAndNonDestructive(t *testing.T) {
	root := t.TempDir()
	if _, _, err := Init(root); err != nil {
		t.Fatal(err)
	}
	custom := filepath.Join(root, ".watchtower", "config.yaml")
	os.WriteFile(custom, []byte("runner: fake\n"), 0o644)
	created, skipped, err := Init(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(created) != 0 {
		t.Fatalf("second init created files: %v", created)
	}
	if len(skipped) == 0 {
		t.Fatal("second init reported no skips")
	}
	b, _ := os.ReadFile(custom)
	if string(b) != "runner: fake\n" {
		t.Fatal("init overwrote existing config.yaml")
	}
}
```

- [ ] **Step 3: Run test to verify it fails**

Run: `go test ./internal/scaffold/`
Expected: FAIL (undefined: Init)

- [ ] **Step 4: Write the implementation**

```go
// Package scaffold writes the embedded default .watchtower tree into a repo.
package scaffold

import (
	"embed"
	"io/fs"
	"os"
	"path/filepath"
)

//go:embed all:defaults
var defaults embed.FS

// Init copies the embedded defaults into <repoRoot>/.watchtower/. It never
// overwrites: existing files are reported in skipped instead.
func Init(repoRoot string) (created, skipped []string, err error) {
	dst := filepath.Join(repoRoot, ".watchtower")
	err = fs.WalkDir(defaults, "defaults", func(path string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		rel, rerr := filepath.Rel("defaults", path)
		if rerr != nil {
			return rerr
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if _, serr := os.Stat(target); serr == nil {
			skipped = append(skipped, rel)
			return nil
		}
		b, rerr := defaults.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		if werr := os.WriteFile(target, b, 0o644); werr != nil {
			return werr
		}
		created = append(created, rel)
		return nil
	})
	return created, skipped, err
}
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./internal/scaffold/`
Expected: PASS

- [ ] **Step 6: Commit**

```bash
git add internal/scaffold/
git commit -m "feat: scaffold package with embedded default flows and packages

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task 4: `watchtower init` and `watchtower repos` commands

**Files:**
- Modify: `cmd/watchtower/main.go` (usage line ~42; switch ~45)

**Interfaces:**
- Consumes: `scaffold.Init`, `repocfg.Register`, `repocfg.ListRegistered`, `repocfg.RepoDataDir` from Tasks 1–3.
- Produces: CLI commands `init` and `repos`. `repos` probes each repo's socket to report `running`/`stopped`.

- [ ] **Step 1: Add the commands**

Add to the usage string: `init|repos`. Add cases to the switch:

```go
case "init":
	fs := flag.NewFlagSet("init", flag.ExitOnError)
	data := fs.String("data", defaultData(), "data dir")
	fs.Parse(args)
	cwd, err := os.Getwd()
	if err != nil {
		fatal(err)
	}
	created, skipped, err := scaffold.Init(cwd)
	if err != nil {
		fatal(err)
	}
	if err := repocfg.Register(*data, cwd); err != nil {
		fatal(err)
	}
	for _, p := range created {
		fmt.Println("created .watchtower/" + p)
	}
	for _, p := range skipped {
		fmt.Println("exists  .watchtower/" + p)
	}
	fmt.Println("registered", cwd)
	fmt.Println("next: run 'watchtower tower' — the daemon starts automatically")
case "repos":
	fs := flag.NewFlagSet("repos", flag.ExitOnError)
	data := fs.String("data", defaultData(), "data dir")
	fs.Parse(args)
	entries, err := repocfg.ListRegistered(*data)
	if err != nil {
		fatal(err)
	}
	for _, e := range entries {
		status := "stopped"
		sock := filepath.Join(repocfg.RepoDataDir(*data, e.Path), "watchtower.sock")
		if conn, err := net.Dial("unix", sock); err == nil {
			conn.Close()
			status = "running"
		}
		fmt.Printf("%-8s %s\n", status, e.Path)
	}
```

Add imports `"github.com/wbushyeager/watchtower/internal/repocfg"` and `"github.com/wbushyeager/watchtower/internal/scaffold"` (`net` is already imported).

- [ ] **Step 2: Verify by hand in a temp repo**

```bash
go build -o /tmp/gh-test ./cmd/watchtower
mkdir -p /tmp/gh-repo && cd /tmp/gh-repo
GH_DATA=$(mktemp -d)
/tmp/gh-test init --data "$GH_DATA"
/tmp/gh-test init --data "$GH_DATA"   # second run: all "exists", nothing clobbered
/tmp/gh-test repos --data "$GH_DATA"  # shows "stopped  /tmp/gh-repo" (path may be /private/tmp/...)
```

Expected: first init prints `created` lines; second prints only `exists` lines; repos lists the repo as stopped.

- [ ] **Step 3: Run the full test suite**

Run: `go test ./...`
Expected: PASS

- [ ] **Step 4: Commit**

```bash
git add cmd/watchtower/main.go
git commit -m "feat: watchtower init and repos commands

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task 5: Daemon reads config with flag precedence; per-repo data dir

**Files:**
- Modify: `cmd/watchtower/main.go` — `runDaemon` (~line 237)

**Interfaces:**
- Consumes: `repocfg.Load`, `repocfg.FindRepo`, `repocfg.RepoDataDir` from Task 1.
- Produces: `watchtower daemon` runs with zero flags inside an initialized repo. `--flows` no longer required. Socket/db/issues live in `<base>/repos/<id>/`. Later tasks rely on the socket path being `repocfg.RepoDataDir(base, repo) + "/watchtower.sock"` and pidfile `daemon.pid` written there.

- [ ] **Step 1: Rework runDaemon**

Replace the top of `runDaemon` (flag parsing through the flows check) with:

```go
func runDaemon(args []string) {
	fs := flag.NewFlagSet("daemon", flag.ExitOnError)
	base := fs.String("data", defaultData(), "base data dir")
	flowsDir := fs.String("flows", "", "flows dir (default from config)")
	slotN := fs.Int("slots", 0, "heavy slots (default from config)")
	runnerKind := fs.String("runner", "", "claude|fake (default from config)")
	repoFlag := fs.String("repo", "", "target repo (default: walk up from CWD)")
	pkgDir := fs.String("packages", "", "agent packages dir (default from config)")
	budget := fs.Int("budget", -1, "per-issue token budget (0=off; default from config)")
	pricePerMTok := fs.Float64("price-per-mtok", -1, "estimated dollars per million tokens (0=hide; default from config)")
	claudeBin := fs.String("claude-bin", "", "claude binary (default from config)")
	testCmd := fs.String("test-cmd", "", "merge-train test command (default from config)")
	fs.Parse(args)

	repo := *repoFlag
	if repo == "" {
		cwd, err := os.Getwd()
		if err != nil {
			fatal(err)
		}
		repo, err = repocfg.FindRepo(cwd)
		if err != nil {
			fatal(err)
		}
	}
	cfg, err := repocfg.Load(repo)
	if err != nil {
		fatal(err)
	}
	// Explicit flags override config; unset flags take config values.
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	if !set["flows"] {
		*flowsDir = cfg.Flows
	}
	if !set["packages"] {
		*pkgDir = cfg.Packages
	}
	if !set["runner"] {
		*runnerKind = cfg.Runner
	}
	if !set["slots"] {
		*slotN = cfg.Slots
	}
	if !set["budget"] {
		*budget = cfg.Budget
	}
	if !set["price-per-mtok"] {
		*pricePerMTok = cfg.PricePerMTok
	}
	if !set["claude-bin"] {
		*claudeBin = cfg.ClaudeBin
	}
	if !set["test-cmd"] {
		*testCmd = cfg.TestCmd
	}

	data := repocfg.RepoDataDir(*base, repo)
	if err := os.MkdirAll(data, 0o755); err != nil {
		fatal(err)
	}
	if err := os.WriteFile(filepath.Join(data, "daemon.pid"), []byte(strconv.Itoa(os.Getpid())), 0o644); err != nil {
		fatal(err)
	}
```

Then adjust the rest of the existing body:
- `store.Open(filepath.Join(data, "watchtower.db"))` — `data` is now the per-repo dir (variable, not flag pointer).
- Flows glob: `filepath.Glob(filepath.Join(*flowsDir, "*.yaml"))`; change the empty-flows error to name the config: `fatal(fmt.Errorf("no flows found in %s (configured in %s)", *flowsDir, repocfg.ConfigPath(repo)))`.
- Remove the old `if *repo == ""` check inside `case "claude":` (repo is always resolved now); replace all `*repo` references with `repo`.
- Engine `DataDir: filepath.Join(data, "issues")`.
- Socket: `sock := filepath.Join(data, "watchtower.sock")` (rest unchanged).
- The `WATCHTOWER_FAKE=1` override stays where it is, after the precedence block.

- [ ] **Step 2: Verify by hand with the fake runner**

```bash
go build -o /tmp/gh-test ./cmd/watchtower
cd /tmp/gh-repo   # from Task 4; has .watchtower/ scaffolded
printf 'runner: fake\n' > .watchtower/config.yaml
/tmp/gh-test daemon --data "$GH_DATA" &
sleep 1
ls "$GH_DATA"/repos/*/watchtower.sock "$GH_DATA"/repos/*/daemon.pid
/tmp/gh-test repos --data "$GH_DATA"   # now shows "running"
kill %1
```

Expected: daemon prints `watchtower daemon listening on <base>/repos/<id>/watchtower.sock`; socket and pidfile exist; repos shows running.

- [ ] **Step 3: Run the full test suite**

Run: `go test ./...`
Expected: PASS

- [ ] **Step 4: Commit**

```bash
git add cmd/watchtower/main.go
git commit -m "feat: daemon reads .watchtower/config.yaml, per-repo data dir

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task 6: Client repo resolution + auto-spawn

**Files:**
- Create: `cmd/watchtower/spawn.go`
- Modify: `cmd/watchtower/main.go` — `mustDial` (~line 448) and every client case (`tower, new, decisions, answer, proposals, accept-proposal, reject-proposal, issues, status, pause, resume, kill, retry, lever, transcript, tail`)

**Interfaces:**
- Consumes: `repocfg.FindRepo`, `repocfg.RepoDataDir` (Task 1); daemon pidfile/socket layout (Task 5).
- Produces: `func mustDial(base, repoFlag string) *proto.Client` — resolves the repo, connects, auto-spawning the daemon if needed. All client commands gain a `--repo` override flag alongside `--data`.

- [ ] **Step 1: Write spawn.go**

```go
package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/wbushyeager/watchtower/internal/proto"
	"github.com/wbushyeager/watchtower/internal/repocfg"
)

// mustDial resolves the target repo, connects to its daemon socket, and
// spawns a detached daemon first if none is listening.
func mustDial(base, repoFlag string) *proto.Client {
	repo := repoFlag
	if repo == "" {
		cwd, err := os.Getwd()
		if err != nil {
			fatal(err)
		}
		repo, err = repocfg.FindRepo(cwd)
		if err != nil {
			fatal(err)
		}
	}
	dataDir := repocfg.RepoDataDir(base, repo)
	sock := filepath.Join(dataDir, "watchtower.sock")
	if c, err := proto.Dial(sock); err == nil {
		return c
	}
	clearStaleSocket(dataDir, sock)
	if err := spawnDaemon(base, repo, dataDir); err != nil {
		fatal(fmt.Errorf("starting daemon: %w", err))
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if c, err := proto.Dial(sock); err == nil {
			fmt.Fprintln(os.Stderr, "started daemon for", repo)
			return c
		}
		time.Sleep(100 * time.Millisecond)
	}
	fatal(fmt.Errorf("daemon did not come up within 5s; last log lines:\n%s",
		tailFile(filepath.Join(dataDir, "daemon.log"), 10)))
	return nil
}

// clearStaleSocket removes a socket left behind by a dead daemon. If the
// pidfile's process is still alive we leave the socket alone (the daemon
// may just be starting up or wedged — the connect retry loop handles it).
func clearStaleSocket(dataDir, sock string) {
	if _, err := os.Stat(sock); err != nil {
		return
	}
	b, err := os.ReadFile(filepath.Join(dataDir, "daemon.pid"))
	if err == nil {
		pid, perr := strconv.Atoi(strings.TrimSpace(string(b)))
		if perr == nil {
			if proc, ferr := os.FindProcess(pid); ferr == nil && proc.Signal(syscall.Signal(0)) == nil {
				return // process alive
			}
		}
	}
	os.Remove(sock)
}

func spawnDaemon(base, repo, dataDir string) error {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return err
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	logf, err := os.OpenFile(filepath.Join(dataDir, "daemon.log"),
		os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer logf.Close()
	cmd := exec.Command(self, "daemon", "--data", base, "--repo", repo)
	cmd.Stdout = logf
	cmd.Stderr = logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} // survive client exit
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}

func tailFile(path string, n int) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return "(no log)"
	}
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
```

- [ ] **Step 2: Update every client command**

In `main.go`, for each client case (all cases except `daemon`, `init`, `repos`): add a repo flag and pass it to `mustDial`. Pattern (shown for `status`; apply identically to `tower`, `new`, `decisions`, `answer`, `proposals`, `accept-proposal`, `reject-proposal`, `issues`, `pause`, `resume`, `kill`, `retry`, `lever`, `transcript`, `tail`):

```go
case "status":
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	data := fs.String("data", defaultData(), "data dir")
	repoF := fs.String("repo", "", "target repo (default: walk up from CWD)")
	fs.Parse(args)
	c := mustDial(*data, *repoF)
	defer c.Close()
	...
```

Note: `tower` already has a `--repo` flag (used for the architecture map); reuse that same flag value for `mustDial(*data, *repo)` — when unset, `mustDial` discovers the repo and `model.Repo` stays `""` as today.

Remove the old `mustDial(data string)` from `main.go` (it moves to `spawn.go` with the new signature). Drop the now-unused `net` import only if nothing else uses it (`repos` from Task 4 uses it — keep).

- [ ] **Step 3: Verify auto-spawn by hand**

```bash
go build -o /tmp/gh-test ./cmd/watchtower
cd /tmp/gh-repo
pkill -f 'gh-test daemon' || true
/tmp/gh-test status --data "$GH_DATA"
# Expected: "started daemon for /tmp/gh-repo" on stderr, then a status line.
/tmp/gh-test status --data "$GH_DATA"
# Expected: no spawn message (daemon reused).
/tmp/gh-test repos --data "$GH_DATA"   # running
pkill -f 'gh-test daemon'
```

- [ ] **Step 4: Run the full test suite**

Run: `go test ./...`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add cmd/watchtower/spawn.go cmd/watchtower/main.go
git commit -m "feat: clients resolve repo from CWD and auto-spawn the daemon

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task 7: Integration test — two repos, no collision, auto-spawn end to end

**Files:**
- Create: `cmd/watchtower/integration_test.go`

**Interfaces:**
- Consumes: the built binary's `init`, `new`, `issues` commands; `runner: fake` config (Task 5); auto-spawn (Task 6).

- [ ] **Step 1: Write the integration test**

```go
package main_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// buildBinary compiles watchtower once into a temp dir.
func buildBinary(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "watchtower")
	cmd := exec.Command("go", "build", "-o", bin, ".")
	cmd.Env = os.Environ()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build: %v: %s", err, out)
	}
	return bin
}

func run(t *testing.T, bin, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v: %v: %s", bin, args, err, out)
	}
	return string(out)
}

func initRepo(t *testing.T, bin, base string) string {
	t.Helper()
	repo := t.TempDir()
	run(t, bin, repo, "init", "--data", base)
	cfg := filepath.Join(repo, ".watchtower", "config.yaml")
	if err := os.WriteFile(cfg, []byte("runner: fake\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return repo
}

func TestTwoReposAutoSpawnWithoutCollision(t *testing.T) {
	bin := buildBinary(t)
	base := t.TempDir()
	repoA := initRepo(t, bin, base)
	repoB := initRepo(t, bin, base)
	t.Cleanup(func() { // kill spawned daemons
		exec.Command("pkill", "-f", bin).Run()
	})

	idA := strings.TrimSpace(lastLine(run(t, bin, repoA, "new", "--data", base, "--title", "issue-a")))
	idB := strings.TrimSpace(lastLine(run(t, bin, repoB, "new", "--data", base, "--title", "issue-b")))
	if idA == "" || idB == "" {
		t.Fatal("issue creation returned empty id")
	}

	// Each repo's daemon sees only its own issue.
	outA := run(t, bin, repoA, "issues", "--data", base)
	outB := run(t, bin, repoB, "issues", "--data", base)
	if !strings.Contains(outA, "issue-a") || strings.Contains(outA, "issue-b") {
		t.Fatalf("repoA issues wrong: %s", outA)
	}
	if !strings.Contains(outB, "issue-b") || strings.Contains(outB, "issue-a") {
		t.Fatalf("repoB issues wrong: %s", outB)
	}

	// Both daemons registered and running.
	repos := run(t, bin, repoA, "repos", "--data", base)
	if strings.Count(repos, "running") != 2 {
		t.Fatalf("expected 2 running daemons:\n%s", repos)
	}
}

// lastLine returns the final non-empty line of s ("new" prints the spawn
// notice to stderr but CombinedOutput merges streams, so take the last line).
func lastLine(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	return lines[len(lines)-1]
}
```

- [ ] **Step 2: Run it**

Run: `go test ./cmd/watchtower/ -run TestTwoRepos -v -timeout 120s`
Expected: PASS. If it fails on the spawn notice polluting stdout parsing, check `lastLine` — the issue ID is the last stdout line from `new`.

- [ ] **Step 3: Run the full test suite**

Run: `go test ./...`
Expected: PASS

- [ ] **Step 4: Commit**

```bash
git add cmd/watchtower/integration_test.go
git commit -m "test: two-repo auto-spawn integration test

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```
