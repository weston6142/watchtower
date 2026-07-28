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
