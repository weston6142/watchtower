package touchset

import (
	"encoding/json"
	"os"
	"strings"
)

// Set is the list of file globs a plan expects to create or modify.
type Set struct {
	Globs []string `json:"globs"`
}

func Load(path string) (Set, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Set{}, err
	}
	var s Set
	if err := json.Unmarshal(b, &s); err != nil {
		return Set{}, err
	}
	return s, nil
}

// prefix strips a glob to its literal leading path (everything before the
// first wildcard), without a trailing slash.
func prefix(glob string) string {
	if i := strings.IndexAny(glob, "*?["); i >= 0 {
		glob = glob[:i]
	}
	return strings.TrimSuffix(glob, "/")
}

// PrefixOf returns the literal leading path of a glob.
func PrefixOf(glob string) string { return prefix(glob) }

// Overlap is conservative: two globs may touch the same file iff one literal
// prefix is a path-prefix of the other.
func Overlap(a, b Set) bool {
	for _, ga := range a.Globs {
		pa := prefix(ga)
		for _, gb := range b.Globs {
			pb := prefix(gb)
			if pathPrefix(pa, pb) || pathPrefix(pb, pa) {
				return true
			}
		}
	}
	return false
}

func pathPrefix(p, of string) bool {
	if p == of {
		return true
	}
	return strings.HasPrefix(of, p+"/") || p == ""
}
