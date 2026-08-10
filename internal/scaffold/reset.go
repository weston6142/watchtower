package scaffold

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/weston6142/watchtower/internal/flow"
	"github.com/weston6142/watchtower/internal/pkgs"
	"github.com/weston6142/watchtower/internal/repocfg"
	"gopkg.in/yaml.v3"
)

type PreparedReplacement struct {
	RepoRoot string
	Staged   string

	tempRoot      string
	oldTree       string
	applied       bool
	finished      bool
	rename        func(string, string) error
	sourceRecheck func() error
}

type PreparedReset = PreparedReplacement

func PrepareReset(repoRoot string) (*PreparedReset, error) {
	return prepareResetFS(repoRoot, defaults, "defaults")
}

func prepareResetFS(repoRoot string, source fs.FS, sourceRoot string) (*PreparedReset, error) {
	absolute, err := filepath.Abs(repoRoot)
	if err != nil {
		return nil, err
	}
	tempRoot, err := os.MkdirTemp(absolute, ".watchtower-reset-*")
	if err != nil {
		return nil, err
	}
	prepared := &PreparedReplacement{
		RepoRoot: absolute, Staged: filepath.Join(tempRoot, ".watchtower"),
		tempRoot: tempRoot, rename: os.Rename,
	}
	if err := copyDefaults(source, sourceRoot, prepared.Staged); err != nil {
		_ = prepared.Cancel()
		return nil, err
	}
	manifest, err := BuildManifest(tempRoot, ManagedFiles(source), DefaultsVersion)
	if err != nil {
		_ = prepared.Cancel()
		return nil, err
	}
	if err := WriteManifest(filepath.Join(prepared.Staged, ProvenanceFile), manifest); err != nil {
		_ = prepared.Cancel()
		return nil, err
	}
	if err := validateResetTree(tempRoot); err != nil {
		_ = prepared.Cancel()
		return nil, err
	}
	return prepared, nil
}

func copyDefaults(source fs.FS, sourceRoot, destination string) error {
	return fs.WalkDir(source, sourceRoot, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(sourceRoot, path)
		if err != nil {
			return err
		}
		target := filepath.Join(destination, relative)
		if entry.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		body, err := fs.ReadFile(source, path)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		return os.WriteFile(target, body, 0o644)
	})
}

func validateResetTree(stagedRoot string) error {
	_, err := ReadManifest(filepath.Join(stagedRoot, ".watchtower", ProvenanceFile))
	if err != nil {
		return fmt.Errorf("validate provenance: %w", err)
	}
	config, err := repocfg.Load(stagedRoot)
	if err != nil {
		return fmt.Errorf("validate config: %w", err)
	}
	packages, err := pkgs.LoadDir(config.Packages)
	if err != nil {
		return fmt.Errorf("validate packages: %w", err)
	}
	packageEntries, err := os.ReadDir(config.Packages)
	if err != nil {
		return fmt.Errorf("validate packages: %w", err)
	}
	for _, entry := range packageEntries {
		if entry.IsDir() {
			if _, ok := packages[entry.Name()]; !ok {
				return fmt.Errorf("validate package %s: missing or unreadable package files", entry.Name())
			}
		}
	}
	entries, err := os.ReadDir(config.Flows)
	if err != nil {
		return fmt.Errorf("validate flows: %w", err)
	}
	loaded := 0
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".yaml") {
			continue
		}
		loaded++
		loadedFlow, err := flow.Load(filepath.Join(config.Flows, entry.Name()))
		if err != nil {
			return fmt.Errorf("validate flow %s: %w", entry.Name(), err)
		}
		for _, stage := range loadedFlow.Stages {
			for _, agent := range stage.Agents {
				if _, ok := packages[agent.Package]; !ok {
					return fmt.Errorf(
						"validate flow %s: stage %s references missing package %s",
						entry.Name(), stage.Name, agent.Package)
				}
			}
		}
	}
	if loaded == 0 {
		return fmt.Errorf("validate flows: no YAML flows found")
	}
	return nil
}

func (p *PreparedReplacement) Apply() error {
	if p == nil || p.finished {
		return fmt.Errorf("reset is not prepared")
	}
	if p.applied {
		return fmt.Errorf("reset is already applied")
	}
	if p.sourceRecheck != nil {
		if err := p.sourceRecheck(); err != nil {
			return err
		}
	}
	target := filepath.Join(p.RepoRoot, ".watchtower")
	oldPlaceholder, err := os.MkdirTemp(p.RepoRoot, ".watchtower-old-*")
	if err != nil {
		return err
	}
	if err := os.Remove(oldPlaceholder); err != nil {
		_ = os.RemoveAll(oldPlaceholder)
		return err
	}
	p.oldTree = oldPlaceholder
	if err := p.rename(target, p.oldTree); err != nil {
		_ = os.RemoveAll(p.oldTree)
		p.oldTree = ""
		return fmt.Errorf("preserve current .watchtower: %w", err)
	}
	if err := p.rename(p.Staged, target); err != nil {
		restoreErr := p.rename(p.oldTree, target)
		if restoreErr != nil {
			return fmt.Errorf("install reset: %v; restore original: %w", err, restoreErr)
		}
		p.oldTree = ""
		return fmt.Errorf("install reset: %w", err)
	}
	p.applied = true
	return nil
}

func (p *PreparedReplacement) SetTestCommand(command string) error {
	if p == nil || p.applied || p.finished {
		return fmt.Errorf("reset is not awaiting configuration")
	}
	if _, err := repocfg.ParseCommand(command); err != nil {
		return fmt.Errorf("test command: %w", err)
	}
	path := filepath.Join(p.Staged, "config.yaml")
	body, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	encodedCommand, err := yaml.Marshal(command)
	if err != nil {
		return err
	}
	scalar := strings.TrimSpace(string(encodedCommand))
	lines := strings.Split(string(body), "\n")
	for index, line := range lines {
		content, comment, _ := strings.Cut(line, "#")
		colon := strings.Index(content, ":")
		if colon < 0 || strings.TrimSpace(content[:colon]) != "test_cmd" {
			continue
		}
		afterColon := content[colon+1:]
		leadingSpace := afterColon[:len(afterColon)-len(strings.TrimLeft(afterColon, " \t"))]
		trailingSpace := afterColon[len(strings.TrimRight(afterColon, " \t")):]
		lines[index] = content[:colon+1] + leadingSpace + scalar + trailingSpace
		if comment != "" {
			lines[index] += "#" + comment
		}
		return os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0o644)
	}
	return fmt.Errorf("reset config has no test_cmd field")
}

func (p *PreparedReplacement) Commit() error {
	if p == nil || !p.applied || p.finished {
		return fmt.Errorf("reset is not applied")
	}
	if err := os.RemoveAll(p.tempRoot); err != nil {
		return err
	}
	if p.oldTree != "" {
		if err := os.RemoveAll(p.oldTree); err != nil {
			return err
		}
	}
	p.finished = true
	p.oldTree = ""
	return nil
}

func (p *PreparedReplacement) Rollback() error {
	if p == nil || !p.applied || p.finished {
		return fmt.Errorf("reset is not applied")
	}
	target := filepath.Join(p.RepoRoot, ".watchtower")
	if err := os.RemoveAll(target); err != nil {
		return err
	}
	if err := p.rename(p.oldTree, target); err != nil {
		return err
	}
	_ = os.RemoveAll(p.tempRoot)
	p.oldTree = ""
	p.finished = true
	return nil
}

func (p *PreparedReplacement) Cancel() error {
	if p == nil || p.finished {
		return nil
	}
	if p.applied {
		return p.Rollback()
	}
	p.finished = true
	return os.RemoveAll(p.tempRoot)
}

func Reset(repoRoot string) error {
	prepared, err := PrepareReset(repoRoot)
	if err != nil {
		return err
	}
	defer prepared.Cancel()
	if err := prepared.Apply(); err != nil {
		return err
	}
	return prepared.Commit()
}
