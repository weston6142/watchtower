package touchset

import (
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
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
	canonical, err := CanonicalGlobs(s.Globs)
	if err != nil {
		return Set{}, err
	}
	s.Globs = canonical
	return s, nil
}

// CanonicalPath returns one safe repository-relative slash path.
func CanonicalPath(value string) (string, error) {
	return canonical(value, false)
}

// CanonicalGlob returns one safe repository-relative slash glob.
func CanonicalGlob(value string) (string, error) {
	return canonical(value, true)
}

func canonical(value string, allowGlob bool) (string, error) {
	if value == "" || value != strings.TrimSpace(value) {
		return "", fmt.Errorf("scope is empty or padded")
	}
	if strings.ContainsRune(value, '\x00') || strings.ContainsRune(value, '\\') {
		return "", fmt.Errorf("scope is not a repository-relative slash path")
	}
	if filepath.IsAbs(value) || path.IsAbs(value) {
		return "", fmt.Errorf("scope is absolute")
	}
	for _, segment := range strings.Split(value, "/") {
		if segment == ".." {
			return "", fmt.Errorf("scope contains traversal")
		}
	}
	cleaned := strings.TrimPrefix(path.Clean(value), "./")
	if cleaned == "" || cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", fmt.Errorf("scope escapes the repository")
	}
	segments := strings.Split(cleaned, "/")
	for _, segment := range segments {
		if segment == ".git" || segment == ".watchtower" {
			return "", fmt.Errorf("scope enters engine-owned path %q", segment)
		}
		if !allowGlob && strings.ContainsAny(segment, "*?[") {
			return "", fmt.Errorf("path contains glob syntax")
		}
		if allowGlob && segment != "**" {
			if _, err := path.Match(segment, segment); err != nil {
				return "", fmt.Errorf("invalid glob segment %q: %w", segment, err)
			}
		}
	}
	if allowGlob && prefix(cleaned) == "" {
		return "", fmt.Errorf("root-wide glob is not allowed")
	}
	return cleaned, nil
}

// CanonicalGlobs validates and de-duplicates scopes without expanding them.
func CanonicalGlobs(globs []string) ([]string, error) {
	result := make([]string, 0, len(globs))
	seen := make(map[string]struct{}, len(globs))
	for _, glob := range globs {
		canonical, err := CanonicalGlob(glob)
		if err != nil {
			return nil, err
		}
		if _, ok := seen[canonical]; ok {
			continue
		}
		seen[canonical] = struct{}{}
		result = append(result, canonical)
	}
	return result, nil
}

// Match reports whether a canonical glob matches a canonical repository path.
func Match(glob, value string) (bool, error) {
	canonicalGlob, err := CanonicalGlob(glob)
	if err != nil {
		return false, err
	}
	canonicalPath, err := CanonicalPath(value)
	if err != nil {
		return false, err
	}
	return matchSegments(strings.Split(canonicalGlob, "/"), strings.Split(canonicalPath, "/")), nil
}

func matchSegments(pattern, value []string) bool {
	if len(pattern) == 0 {
		return len(value) == 0
	}
	if pattern[0] == "**" {
		if len(pattern) == 1 {
			return len(value) > 0
		}
		if len(value) == 0 {
			return matchSegments(pattern[1:], value)
		}
		return matchSegments(pattern[1:], value) || matchSegments(pattern, value[1:])
	}
	if len(value) == 0 {
		return false
	}
	matched, err := path.Match(pattern[0], value[0])
	return err == nil && matched && matchSegments(pattern[1:], value[1:])
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
