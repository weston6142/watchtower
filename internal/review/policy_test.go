package review_test

import (
	"testing"

	"github.com/weston6142/watchtower/internal/flow"
	"github.com/weston6142/watchtower/internal/review"
)

func TestResolvePlanReviewPolicy(t *testing.T) {
	cases := []struct {
		name                string
		mode                flow.Lever
		settings            review.PolicySettings
		wantHuman, wantAuto bool
		wantReason          string
	}{
		{
			name: "regular defaults to human",
			mode: flow.LeverRegular,
			settings: review.PolicySettings{
				ID: "manual-default", Version: "1", Valid: true,
			},
			wantHuman: true, wantReason: "manual_default",
		},
		{
			name: "regular explicit policy auto approval",
			mode: flow.LeverRegular,
			settings: review.PolicySettings{
				ID: "team-ci", Version: "2026-08-03", AutoApproveRegular: true, Valid: true,
			},
			wantAuto: true, wantReason: "policy_opt_in",
		},
		{
			name: "strict overrides auto approval",
			mode: flow.LeverStrict,
			settings: review.PolicySettings{
				ID: "team-ci", Version: "2026-08-03", AutoApproveRegular: true, Valid: true,
			},
			wantHuman: true, wantReason: "strict_mode",
		},
		{
			name:      "missing identity fails closed",
			mode:      flow.LeverRegular,
			settings:  review.PolicySettings{AutoApproveRegular: true, Valid: true},
			wantHuman: true, wantReason: "invalid_policy",
		},
		{
			name: "invalid configuration fails closed",
			mode: flow.LeverRegular,
			settings: review.PolicySettings{
				ID: "team-ci", Version: "2026-08-03", AutoApproveRegular: true,
			},
			wantHuman: true, wantReason: "invalid_policy",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := review.ResolvePlanReviewPolicy(tc.mode, tc.settings)
			if got.HumanRequired != tc.wantHuman || got.PolicyAutoApproval != tc.wantAuto || got.Reason != tc.wantReason {
				t.Fatalf("policy = %+v, want human=%v auto=%v reason=%q", got, tc.wantHuman, tc.wantAuto, tc.wantReason)
			}
		})
	}
}
