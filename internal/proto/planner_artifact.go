package proto

import (
	"fmt"
	"sync/atomic"

	"github.com/weston6142/watchtower/internal/plannerartifact"
)

func plannerBinding(cmd Command) plannerartifact.Binding {
	return plannerartifact.Binding{
		IssueID:  cmd.IssueID,
		Stage:    cmd.Stage,
		Attempt:  cmd.Attempt,
		Worktree: cmd.Worktree,
	}
}

func (sv *Server) plannerCorrelation() string {
	return fmt.Sprintf("planner-%d", atomic.AddUint64(&sv.plannerCorrelationCounter, 1))
}

func (sv *Server) plannerFailure(err error) Response {
	class := plannerartifact.ErrorClassOf(err)
	if class == "" {
		class = plannerartifact.ErrorAuthorityState
	}
	message := "planner authority: " + string(class)
	if err != nil {
		message = err.Error()
	}
	return Response{
		Error:         message,
		ErrorClass:    string(class),
		CorrelationID: sv.plannerCorrelation(),
	}
}

func (sv *Server) plannerExec(cmd Command) Response {
	scope := plannerBinding(cmd)
	switch cmd.Op {
	case "planner_authority_issue":
		if sv.eng == nil {
			return sv.plannerFailure(plannerartifact.NewAuthorityError(plannerartifact.ErrorAuthorityUninitialized, "", "engine authority is unavailable"))
		}
		handle, binding, err := sv.eng.IssuePlannerAuthority(scope)
		if err != nil {
			return sv.plannerFailure(err)
		}
		return Response{OK: true, PlannerHandle: handle, IssueID: binding.IssueID,
			CorrelationID: sv.plannerCorrelation()}
	case "apply_planner_artifact":
		request := cmd.PlannerRequest
		if request == nil {
			request = cmd.PlannerArtifact
		}
		if request == nil {
			return sv.plannerFailure(plannerartifact.NewAuthorityError(plannerartifact.ErrorInvalidSection, "", "planner request is missing"))
		}
		if sv.eng == nil {
			return sv.plannerFailure(plannerartifact.NewAuthorityError(plannerartifact.ErrorAuthorityUninitialized, "", "engine authority is unavailable"))
		}
		handle := cmd.PlannerHandle
		if handle == "" {
			var issueErr error
			handle, _, issueErr = sv.eng.IssuePlannerAuthority(scope)
			if issueErr != nil {
				return sv.plannerFailure(issueErr)
			}
		}
		key, err := sv.eng.ApplyPlannerArtifact(scope, handle, *request)
		if err != nil {
			response := sv.plannerFailure(err)
			response.SectionKey = request.Key
			return response
		}
		return Response{OK: true, SectionKey: key, CorrelationID: sv.plannerCorrelation()}
	case "planner_authority_retry":
		if sv.eng == nil {
			return sv.plannerFailure(plannerartifact.NewAuthorityError(plannerartifact.ErrorAuthorityUninitialized, "", "engine authority is unavailable"))
		}
		handle, err := sv.eng.RetryPlannerAuthority(scope)
		if err != nil {
			return sv.plannerFailure(err)
		}
		return Response{OK: true, PlannerHandle: handle, CorrelationID: sv.plannerCorrelation()}
	case "planner_authority_validate_complete":
		if sv.eng == nil {
			return sv.plannerFailure(plannerartifact.NewAuthorityError(plannerartifact.ErrorAuthorityUninitialized, "", "engine authority is unavailable"))
		}
		if err := sv.eng.ValidatePlannerAuthority(scope, cmd.PlannerHandle); err != nil {
			return sv.plannerFailure(err)
		}
		return Response{OK: true, CorrelationID: sv.plannerCorrelation()}
	case "planner_authority_expire":
		if sv.eng == nil {
			return sv.plannerFailure(plannerartifact.NewAuthorityError(plannerartifact.ErrorAuthorityUninitialized, "", "engine authority is unavailable"))
		}
		if err := sv.eng.ExpirePlannerAuthority(scope); err != nil {
			return sv.plannerFailure(err)
		}
		return Response{OK: true, CorrelationID: sv.plannerCorrelation()}
	default:
		return Response{Error: "unknown planner authority operation"}
	}
}
