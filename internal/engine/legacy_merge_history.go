package engine

import (
	"fmt"
	"os/exec"
	"strings"
)

func resolveLegacyBaseSHA(repo, baseBranch string) (string, error) {
	if repo == "" || baseBranch == "" {
		return "", fmt.Errorf("repository and base branch are required")
	}
	resolved, err := resolveLegacyCommit(repo, baseBranch+"^{commit}")
	if err != nil {
		return "", err
	}
	return resolved, nil
}

func matchCanonicalLegacyMerge(repo, baseSHA, baseBranch, issueBranch string) string {
	if repo == "" || !isLegacyCommitID(baseSHA) || baseBranch == "" || issueBranch == "" {
		return ""
	}

	cmd := exec.Command("git", "-C", repo, "log", "--format=%H%x00%s", "--no-decorate", "--no-color", baseSHA, "--")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return ""
	}
	wantSubject := fmt.Sprintf("Merge branch '%s' into %s", issueBranch, baseBranch)
	var matches []string
	for _, line := range strings.Split(string(out), "\n") {
		if line == "" {
			continue
		}
		separator := strings.IndexByte(line, 0)
		if separator < 0 {
			return ""
		}
		candidate := line[:separator]
		subject := line[separator+1:]
		if !isLegacyCommitID(candidate) || subject != wantSubject {
			continue
		}

		resolved, err := resolveLegacyCommit(repo, candidate+"^{commit}")
		if err != nil || resolved != candidate {
			continue
		}
		parents := exec.Command("git", "-C", repo, "rev-list", "--parents", "-n", "1", candidate)
		parentOutput, err := parents.CombinedOutput()
		if err != nil {
			continue
		}
		fields := strings.Fields(string(parentOutput))
		if len(fields) != 3 || fields[0] != candidate ||
			!isLegacyCommitID(fields[1]) || !isLegacyCommitID(fields[2]) {
			continue
		}

		reachable := exec.Command("git", "-C", repo, "merge-base", "--is-ancestor", candidate, baseSHA)
		if err := reachable.Run(); err != nil {
			continue
		}
		matches = append(matches, candidate)
	}
	if len(matches) != 1 {
		return ""
	}
	return matches[0]
}

func resolveLegacyCommit(repo, object string) (string, error) {
	cmd := exec.Command("git", "-C", repo, "rev-parse", "--verify", "--end-of-options", object)
	out, err := cmd.CombinedOutput()
	resolved := strings.TrimSpace(string(out))
	if err != nil {
		return "", fmt.Errorf("resolve %s: %v: %s", object, err, resolved)
	}
	if strings.ContainsAny(resolved, "\r\n") || !isLegacyCommitID(resolved) {
		return "", fmt.Errorf("resolve %s returned a non-canonical commit ID", object)
	}
	return resolved, nil
}

func isLegacyCommitID(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	for _, r := range value {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') && (r < 'A' || r > 'F') {
			return false
		}
	}
	return true
}
