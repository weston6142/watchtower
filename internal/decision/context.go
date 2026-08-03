package decision

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"unicode"
)

// MaxMessageBytes is the maximum serialized size for a newline-delimited
// Watchtower message.
const MaxMessageBytes = 1 << 20

// AgentIdentity is the human-readable identity supplied by an agent package.
type AgentIdentity struct {
	Name   string `json:"name" yaml:"name"`
	Color  string `json:"color" yaml:"color"`
	Symbol string `json:"symbol" yaml:"symbol"`
}

// DecisionContext is the trusted context shown with a newly-created decision.
type DecisionContext struct {
	TaskSummary string `json:"task_summary"`
	AgentName   string `json:"agent_name"`
	AgentColor  string `json:"agent_color"`
	AgentSymbol string `json:"agent_symbol"`
}

var hexColor = regexp.MustCompile(`^(?:#|0x)[0-9a-fA-F]+$`)

// BuildTaskSummary freezes the first sentence from each nonblank issue field.
func BuildTaskSummary(title, body string) (string, error) {
	titleSentence := firstSentence(title)
	bodySentence := firstSentence(body)
	if titleSentence == "" && bodySentence == "" {
		return "", fmt.Errorf("issue title and body are blank")
	}
	parts := make([]string, 0, 2)
	if titleSentence != "" {
		parts = append(parts, titleSentence)
	}
	if bodySentence != "" {
		parts = append(parts, bodySentence)
	}
	return strings.Join(parts, ": ") + ".", nil
}

func firstSentence(value string) string {
	fields := strings.Fields(value)
	if len(fields) == 0 {
		return ""
	}
	normalized := strings.Join(fields, " ")
	for i, r := range normalized {
		if r == '.' || r == '!' || r == '?' {
			return strings.TrimSpace(normalized[:i])
		}
	}
	return strings.TrimSpace(normalized)
}

// ValidateAgentIdentity validates package-provided identity metadata.
func ValidateAgentIdentity(identity AgentIdentity) error {
	if err := validateText("name", identity.Name); err != nil {
		return err
	}
	if err := validateText("color", identity.Color); err != nil {
		return err
	}
	if strings.ContainsRune(identity.Color, '\x1b') || hexColor.MatchString(strings.TrimSpace(identity.Color)) {
		return fmt.Errorf("invalid color: display escape or encoded color is not allowed")
	}
	if err := validateText("symbol", identity.Symbol); err != nil {
		return err
	}
	return nil
}

// ValidateDecisionContext validates a complete context and its serialized
// size without changing any supplied value.
func ValidateDecisionContext(context DecisionContext) error {
	if err := validateText("task_summary", context.TaskSummary); err != nil {
		return err
	}
	if err := validateText("agent_name", context.AgentName); err != nil {
		return err
	}
	if err := validateText("agent_color", context.AgentColor); err != nil {
		return err
	}
	if strings.ContainsRune(context.AgentColor, '\x1b') || hexColor.MatchString(strings.TrimSpace(context.AgentColor)) {
		return fmt.Errorf("invalid agent_color: display escape or encoded color is not allowed")
	}
	if err := validateText("agent_symbol", context.AgentSymbol); err != nil {
		return err
	}
	encoded, err := json.Marshal(context)
	if err != nil {
		return fmt.Errorf("marshal decision context: %w", err)
	}
	if len(encoded) > MaxMessageBytes {
		return fmt.Errorf("decision context exceeds %d-byte message budget", MaxMessageBytes)
	}
	return nil
}

func validateText(field, value string) error {
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("invalid %s: value must not be blank", field)
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return fmt.Errorf("invalid %s: control characters are not allowed", field)
		}
	}
	return nil
}
