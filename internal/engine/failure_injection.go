package engine

import (
	"context"
	"fmt"

	"github.com/weston6142/watchtower/internal/failure"
)

type FailureInjector interface {
	BeforeWorkspace(context.Context, string, string) error
	BeforeArtifact(context.Context, string, string) error
	BeforePlanner(context.Context, string, string) error
	BeforeGit(context.Context, string, string) error
	BeforeVerification(context.Context, string, string) error
	BeforeCache(context.Context, string, string) error
	BeforeStore(context.Context, string, string) error
	BeforeFinalization(context.Context, string, string) error
}

type injectedSubsystemFailure struct {
	site failure.Site
	err  error
}

func (e *injectedSubsystemFailure) Error() string { return e.err.Error() }
func (e *injectedSubsystemFailure) Unwrap() error { return e.err }

func (e *Engine) injectFailure(ctx context.Context, site failure.Site, issueID, stage string) error {
	if e.cfg.FailureInjector == nil {
		return nil
	}
	var err error
	switch site {
	case failure.SiteWorkspace:
		err = e.cfg.FailureInjector.BeforeWorkspace(ctx, issueID, stage)
	case failure.SiteArtifact:
		err = e.cfg.FailureInjector.BeforeArtifact(ctx, issueID, stage)
	case failure.SitePlanner:
		err = e.cfg.FailureInjector.BeforePlanner(ctx, issueID, stage)
	case failure.SiteGit:
		err = e.cfg.FailureInjector.BeforeGit(ctx, issueID, stage)
	case failure.SiteVerification:
		err = e.cfg.FailureInjector.BeforeVerification(ctx, issueID, stage)
	case failure.SiteCache:
		err = e.cfg.FailureInjector.BeforeCache(ctx, issueID, stage)
	case failure.SiteStore:
		err = e.cfg.FailureInjector.BeforeStore(ctx, issueID, stage)
	case failure.SiteFinalization:
		err = e.cfg.FailureInjector.BeforeFinalization(ctx, issueID, stage)
	default:
		return fmt.Errorf("unsupported failure injection site %q", site)
	}
	if err == nil {
		return nil
	}
	return &injectedSubsystemFailure{site: site, err: err}
}
