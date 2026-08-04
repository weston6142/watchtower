package plannerbudget

import (
	"errors"
	"math"
	"time"

	"github.com/weston6142/watchtower/internal/stageusage"
)

type Profile struct {
	Calls   stageusage.DimensionLimit `json:"calls" yaml:"calls"`
	Tokens  stageusage.DimensionLimit `json:"tokens" yaml:"tokens"`
	Elapsed stageusage.ElapsedLimit   `json:"elapsed" yaml:"elapsed"`
}

type DimensionOverride struct {
	Warning *int64 `json:"warn,omitempty" yaml:"warn,omitempty"`
	Hard    *int64 `json:"hard,omitempty" yaml:"hard,omitempty"`
}

type ElapsedOverride struct {
	Warning *time.Duration `json:"warn,omitempty" yaml:"warn,omitempty"`
	Hard    *time.Duration `json:"hard,omitempty" yaml:"hard,omitempty"`
}

type Override struct {
	Calls   *DimensionOverride `json:"calls,omitempty" yaml:"calls,omitempty"`
	Tokens  *DimensionOverride `json:"tokens,omitempty" yaml:"tokens,omitempty"`
	Elapsed *ElapsedOverride   `json:"elapsed,omitempty" yaml:"elapsed,omitempty"`
}

const (
	defaultCallsWarning   int64         = 24
	defaultCallsHard      int64         = 32
	defaultTokensWarning  int64         = 200000
	defaultTokensHard     int64         = 250000
	defaultElapsedWarning time.Duration = 8 * time.Minute
	defaultElapsedHard    time.Duration = 10 * time.Minute
)

func DefaultProfile() Profile {
	return Profile{
		Calls:   stageusage.DimensionLimit{Warning: defaultCallsWarning, Hard: defaultCallsHard},
		Tokens:  stageusage.DimensionLimit{Warning: defaultTokensWarning, Hard: defaultTokensHard},
		Elapsed: stageusage.ElapsedLimit{Warning: defaultElapsedWarning, Hard: defaultElapsedHard},
	}
}

func (p Profile) Validate() error {
	if err := validateDimension(p.Calls); err != nil {
		return err
	}
	if err := validateDimension(p.Tokens); err != nil {
		return err
	}
	if p.Elapsed.Warning <= 0 || p.Elapsed.Hard <= 0 ||
		p.Elapsed.Warning == time.Duration(math.MaxInt64) || p.Elapsed.Hard == time.Duration(math.MaxInt64) ||
		p.Elapsed.Warning >= p.Elapsed.Hard {
		return errors.New("planner elapsed limits must be positive and warning must be below hard")
	}
	return nil
}

func validateDimension(limit stageusage.DimensionLimit) error {
	if limit.Warning <= 0 || limit.Hard <= 0 || limit.Warning >= limit.Hard ||
		limit.Warning == math.MaxInt64 || limit.Hard == math.MaxInt64 {
		return errors.New("planner limits must be finite and warning must be below hard")
	}
	return nil
}

func Resolve(base Profile, override *Override) (Profile, error) {
	if base == (Profile{}) {
		base = DefaultProfile()
	}
	if err := base.Validate(); err != nil {
		return Profile{}, err
	}
	if override == nil {
		return base, nil
	}
	resolved := base
	applyDimension := func(dst *stageusage.DimensionLimit, src *DimensionOverride) {
		if src == nil {
			return
		}
		if src.Warning != nil {
			dst.Warning = *src.Warning
		}
		if src.Hard != nil {
			dst.Hard = *src.Hard
		}
	}
	applyDimension(&resolved.Calls, override.Calls)
	applyDimension(&resolved.Tokens, override.Tokens)
	if override.Elapsed != nil {
		if override.Elapsed.Warning != nil {
			resolved.Elapsed.Warning = *override.Elapsed.Warning
		}
		if override.Elapsed.Hard != nil {
			resolved.Elapsed.Hard = *override.Elapsed.Hard
		}
	}
	if err := resolved.Validate(); err != nil {
		return Profile{}, err
	}
	return resolved, nil
}
