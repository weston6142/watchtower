package plannerartifact

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

// ErrorClass is the stable, operator-safe failure class used across the
// planner authority boundary. It deliberately carries no capability or
// request data.
type ErrorClass string

const (
	ErrorTransportUnavailable   ErrorClass = "transport_unavailable"
	ErrorAuthorityUninitialized ErrorClass = "authority_uninitialized"
	ErrorScopeMismatch          ErrorClass = "scope_mismatch"
	ErrorStaleCapability        ErrorClass = "stale_capability"
	ErrorPrivateSession         ErrorClass = "private_session_rejected"
	ErrorDescriptor             ErrorClass = "descriptor_non_authoritative"
	ErrorInvalidSection         ErrorClass = "invalid_section"
	ErrorAuthorityState         ErrorClass = "authority_state_unavailable"
)

// AuthorityError is safe to cross a process boundary. Key is an immutable
// manifest key, not user-authored section content.
type AuthorityError struct {
	Class  ErrorClass
	Key    string
	Reason string
	Cause  error
}

func (e *AuthorityError) Error() string {
	parts := []string{string(e.Class)}
	if e.Key != "" {
		parts = append(parts, "key="+e.Key)
	}
	if e.Reason != "" {
		parts = append(parts, e.Reason)
	}
	return "planner authority: " + strings.Join(parts, " ")
}

func (e *AuthorityError) Unwrap() error { return e.Cause }

func authorityError(class ErrorClass, key, reason string, cause error) error {
	return &AuthorityError{Class: class, Key: key, Reason: reason, Cause: cause}
}

// ErrorClassOf maps planner failures to the stable boundary class. The
// fallback is intentionally conservative: unknown failures never claim a
// successful or authoritative operation.
func ErrorClassOf(err error) ErrorClass {
	if err == nil {
		return ""
	}
	var authorityErr *AuthorityError
	if errors.As(err, &authorityErr) {
		return authorityErr.Class
	}
	var diagnostic *DiagnosticError
	if errors.As(err, &diagnostic) {
		if diagnostic.Scope == ScopeTransport {
			return ErrorTransportUnavailable
		}
		return ErrorInvalidSection
	}
	return ErrorAuthorityState
}

func validateFinalSection(markdown string) error {
	if !utf8.ValidString(markdown) {
		return fmt.Errorf("section is not valid UTF-8")
	}
	trimmed := strings.TrimSpace(markdown)
	if trimmed == "" {
		return fmt.Errorf("section is empty")
	}
	// Bracket-only templates are probes too, while ordinary prose containing
	// these words remains valid.
	probe := strings.ToLower(strings.TrimSpace(strings.Trim(trimmed, "[](){}<>")))
	switch probe {
	case "todo", "tbd", "fixme", "placeholder", "transport probe":
		return fmt.Errorf("section is placeholder content")
	default:
		return nil
	}
}
