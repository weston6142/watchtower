package decision

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestBuildTaskSummary(t *testing.T) {
	tests := []struct {
		name  string
		title string
		body  string
		want  string
	}{
		{
			name:  "title and body use first sentence",
			title: "  Make   prompts self-identifying! More title detail",
			body:  "  Keep every decision readable. Additional body detail",
			want:  "Make prompts self-identifying: Keep every decision readable.",
		},
		{
			name:  "title only",
			title: "Preserve the complete task context.",
			want:  "Preserve the complete task context.",
		},
		{
			name: "body only",
			body: "  Explain the behavior clearly. More detail follows",
			want: "Explain the behavior clearly.",
		},
		{
			name:  "long values are not shortened",
			title: "Deliver " + strings.Repeat("complete ", 300) + "context",
			want:  "Deliver " + strings.Repeat("complete ", 300) + "context.",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := BuildTaskSummary(tt.title, tt.body)
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Fatalf("summary = %q, want %q", got, tt.want)
			}
			if strings.Contains(got, "...") {
				t.Fatal("summary was shortened")
			}
		})
	}
}

func TestBuildTaskSummaryRejectsBlankIssueMaterial(t *testing.T) {
	for _, tt := range []struct {
		name  string
		title string
		body  string
	}{
		{name: "both blank"},
		{name: "only whitespace", title: " \t\n ", body: " \n\t"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := BuildTaskSummary(tt.title, tt.body); err == nil {
				t.Fatal("blank issue material was accepted")
			}
		})
	}
}

func TestValidateDecisionContext(t *testing.T) {
	valid := DecisionContext{
		TaskSummary: "Make decision prompts self-identifying.",
		AgentName:   "Executor",
		AgentColor:  "green",
		AgentSymbol: "⚙",
	}
	if err := ValidateDecisionContext(valid); err != nil {
		t.Fatalf("valid context rejected: %v", err)
	}

	tests := []struct {
		name string
		edit func(*DecisionContext)
		want string
	}{
		{name: "blank task summary", edit: func(c *DecisionContext) { c.TaskSummary = " \t" }, want: "task_summary"},
		{name: "blank agent name", edit: func(c *DecisionContext) { c.AgentName = "" }, want: "agent_name"},
		{name: "blank agent color", edit: func(c *DecisionContext) { c.AgentColor = "\n" }, want: "agent_color"},
		{name: "blank agent symbol", edit: func(c *DecisionContext) { c.AgentSymbol = " " }, want: "agent_symbol"},
		{name: "control character in name", edit: func(c *DecisionContext) { c.AgentName = "Exec\x1butor" }, want: "agent_name"},
		{name: "ansi color", edit: func(c *DecisionContext) { c.AgentColor = "\x1b[32mgreen" }, want: "agent_color"},
		{name: "hex color", edit: func(c *DecisionContext) { c.AgentColor = "#00ff00" }, want: "agent_color"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := valid
			tt.edit(&ctx)
			if err := ValidateDecisionContext(ctx); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want field %q", err, tt.want)
			}
		})
	}
}

func TestValidateDecisionContextAcceptsUnicodeIdentity(t *testing.T) {
	ctx := DecisionContext{
		TaskSummary: "Keep the complete task context.",
		AgentName:   "Correctness Reviewer",
		AgentColor:  "sunset orange",
		AgentSymbol: "⛨",
	}
	if err := ValidateDecisionContext(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestValidateDecisionContextEnforcesSerializedBudgetWithoutTruncation(t *testing.T) {
	base := DecisionContext{
		TaskSummary: "Task.",
		AgentName:   "Agent",
		AgentColor:  "green",
		AgentSymbol: "⚙",
	}
	empty := base
	empty.TaskSummary = ""
	emptyEncoded, err := json.Marshal(empty)
	if err != nil {
		t.Fatal(err)
	}
	base.TaskSummary = strings.Repeat("x", MaxMessageBytes-len(emptyEncoded))
	encoded, err := json.Marshal(base)
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded) != MaxMessageBytes {
		t.Fatalf("boundary payload = %d bytes, want %d", len(encoded), MaxMessageBytes)
	}
	if err := ValidateDecisionContext(base); err != nil {
		t.Fatalf("boundary context rejected: %v", err)
	}

	tooLarge := base
	tooLarge.TaskSummary += "x"
	if err := ValidateDecisionContext(tooLarge); err == nil {
		t.Fatal("over-budget context was accepted")
	}
	if !strings.HasSuffix(tooLarge.TaskSummary, "x") {
		t.Fatal("over-budget input was truncated")
	}
}
