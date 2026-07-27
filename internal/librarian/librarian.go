package librarian

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Librarian curates project memory: a directory of markdown files injected
// into every new issue's context.
type Librarian struct {
	MemoryDir string
}

func (l *Librarian) Context() (string, error) {
	entries, err := os.ReadDir(l.MemoryDir)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".md") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	var b strings.Builder
	for _, n := range names {
		content, err := os.ReadFile(filepath.Join(l.MemoryDir, n))
		if err != nil {
			return "", err
		}
		fmt.Fprintf(&b, "## %s\n\n%s\n\n", n, strings.TrimSpace(string(content)))
	}
	return strings.TrimSpace(b.String()), nil
}
