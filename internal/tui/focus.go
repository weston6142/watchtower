package tui

import (
	"sort"

	"github.com/weston6142/watchtower/internal/projection"
)

func resolveFocus(f Focus, st *projection.State, stages []string, retired map[string]bool) Focus {
	if f.Floor < 0 {
		f.Floor = 0
	}
	if f.Floor > len(stages) {
		f.Floor = len(stages)
	}
	cards := floorCards(st, stages, f.Floor, retired)
	if len(cards) == 0 {
		f.Card = 0
		f.Issue = ""
		return f
	}
	if f.Card < 0 {
		f.Card = 0
	}
	if f.Card >= len(cards) {
		f.Card = len(cards) - 1
	}
	f.Issue = cards[f.Card]
	return f
}

func focusIssue(st *projection.State, stages []string, issueID string, retired map[string]bool) Focus {
	for floor := 1; floor <= len(stages); floor++ {
		for card, id := range floorCards(st, stages, floor, retired) {
			if id == issueID {
				return Focus{Floor: floor, Card: card, Issue: issueID}
			}
		}
	}
	return Focus{Issue: issueID}
}

// moveFocus moves one flip at a time through the rendered floors and cards.
func moveFocus(f Focus, st *projection.State, stages []string, key string, retired map[string]bool) Focus {
	f = resolveFocus(f, st, stages, retired)
	switch key {
	case "j":
		f.Floor++
		f.Card = 0
		return resolveFocus(f, st, stages, retired)
	case "k":
		f.Floor--
		f.Card = 0
		return resolveFocus(f, st, stages, retired)
	case "h":
		f.Card--
		return resolveFocus(f, st, stages, retired)
	case "l":
		f.Card++
		return resolveFocus(f, st, stages, retired)
	case "g":
		return resolveFocus(Focus{Floor: 0}, st, stages, retired)
	case "tab":
		items := attentionList(st)
		if len(items) == 0 {
			return f
		}
		current := -1
		for i, id := range items {
			if id == f.Issue {
				current = i
				break
			}
		}
		return focusIssue(st, stages, items[(current+1)%len(items)], retired)
	default:
		if len(key) == 1 && key >= "1" && key <= "9" {
			index := int(key[0] - '1')
			if lanes := visibleOrder(st, retired); index < len(lanes) {
				return focusIssue(st, stages, lanes[index], retired)
			}
		}
	}
	return f
}

// attentionList returns attention issues by severity, stable within a class.
func attentionList(st *projection.State) []string {
	if st == nil {
		return nil
	}
	severity := func(id string) int {
		iv := st.Issues[id]
		switch iv.State {
		case "failed":
			return 0
		case "waiting_decision":
			return 1
		case "queued_for_slot":
			return 2
		default:
			return 99
		}
	}
	items := make([]string, 0, len(st.Order))
	for _, id := range st.Order {
		if severity(id) < 99 {
			items = append(items, id)
		}
	}
	sort.SliceStable(items, func(i, j int) bool {
		return severity(items[i]) < severity(items[j])
	})
	return items
}
