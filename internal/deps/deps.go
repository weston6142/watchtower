package deps

import "fmt"

// Graph maps each known issue to the issues that must merge before it starts.
type Graph map[string][]string

func (g Graph) Validate() error {
	for issueID, parents := range g {
		seen := map[string]bool{}
		for _, parent := range parents {
			if parent == issueID {
				return fmt.Errorf("issue %s cannot depend on itself", issueID)
			}
			if seen[parent] {
				return fmt.Errorf("issue %s repeats dependency %s", issueID, parent)
			}
			seen[parent] = true
			if _, ok := g[parent]; !ok {
				return fmt.Errorf("issue %s depends on unknown issue %s", issueID, parent)
			}
		}
	}
	const (
		unseen = iota
		visiting
		visited
	)
	state := map[string]int{}
	var visit func(string) error
	visit = func(issueID string) error {
		switch state[issueID] {
		case visiting:
			return fmt.Errorf("dependency cycle includes %s", issueID)
		case visited:
			return nil
		}
		state[issueID] = visiting
		for _, parent := range g[issueID] {
			if err := visit(parent); err != nil {
				return err
			}
		}
		state[issueID] = visited
		return nil
	}
	for issueID := range g {
		if err := visit(issueID); err != nil {
			return err
		}
	}
	return nil
}
