package claude

import "testing"

func TestEffortEnv(t *testing.T) {
	cases := map[string]string{
		"low":    "MAX_THINKING_TOKENS=1024",
		"medium": "MAX_THINKING_TOKENS=8192",
		"high":   "MAX_THINKING_TOKENS=32768",
		"":       "",
		"weird":  "",
	}
	for in, want := range cases {
		if got := EffortEnv(in); got != want {
			t.Errorf("EffortEnv(%q) = %q, want %q", in, got, want)
		}
	}
}
