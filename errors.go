package main

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/emersion/go-smtp"
)

// zulipError carries what Zulip said, so the SMTP layer can translate it.
type zulipError struct {
	status int    // HTTP status
	code   string // Zulip's own error code, e.g. RATE_LIMIT_HIT
	msg    string // Zulip's human-readable message
}

func (e *zulipError) Error() string {
	if e.code != "" {
		return fmt.Sprintf("zulip %d %s: %s", e.status, e.code, e.msg)
	}
	return fmt.Sprintf("zulip %d: %s", e.status, e.msg)
}

// isNoSuchUser reports whether Zulip clearly said the address is not a user
// here -- as opposed to the request failing for some other reason.
func isNoSuchUser(err error) bool {
	var ze *zulipError
	if !errors.As(err, &ze) {
		return false
	}
	return ze.status == http.StatusNotFound || ze.status == http.StatusBadRequest
}

// toSMTP maps a Zulip failure onto an SMTP reply.
//
// ★ THIS TABLE IS THE RELIABILITY DESIGN, not bookkeeping.
//
// SMTP splits failures into permanent (5xx -- the client gives up) and transient
// (4xx -- the client retries later). Get the rate-limit row wrong and a brief
// Zulip hiccup silently loses somebody's password reset, because Keycloak will
// treat a 5xx as final and never try again.
//
//	401  unauthorized      -> 535  permanent. The key is wrong; retrying will not help.
//	404/400 no such user   -> 550  permanent. The address is not a Zulip user.
//	429  rate limited      -> 451  TRANSIENT. Back off and retry.
//	5xx  / unreachable     -> 451  TRANSIENT. Zulip's problem, not the caller's.
func (e *zulipError) toSMTP() *smtp.SMTPError {
	switch {
	// ★ Rate limiting is tested FIRST, before any status-based rule.
	//
	// Zulip sends 429 today, but it is entitled to report a rate limit with
	// RATE_LIMIT_HIT alongside some other status. If that were matched by the
	// 400 rule below it would become a permanent 550 and the message would be
	// dropped rather than retried -- the exact failure this table exists to
	// prevent. Order matters here; do not sort these cases for tidiness.
	case e.status == http.StatusTooManyRequests || e.code == "RATE_LIMIT_HIT":
		return &smtp.SMTPError{
			Code:         451,
			EnhancedCode: smtp.EnhancedCode{4, 4, 5},
			Message:      "Zulip is rate limiting; try again shortly",
		}

	case e.status == http.StatusUnauthorized:
		return &smtp.SMTPError{
			Code:         535,
			EnhancedCode: smtp.EnhancedCode{5, 7, 8},
			Message:      "Zulip rejected these bot credentials",
		}

	case e.status == http.StatusNotFound || e.status == http.StatusBadRequest:
		return &smtp.SMTPError{
			Code:         550,
			EnhancedCode: smtp.EnhancedCode{5, 1, 1},
			Message:      "Zulip rejected the recipient: " + e.msg,
		}

	default:
		return &smtp.SMTPError{
			Code:         451,
			EnhancedCode: smtp.EnhancedCode{4, 4, 1},
			Message:      "Zulip is unavailable; try again shortly",
		}
	}
}

var errAuthRequired = &smtp.SMTPError{
	Code:         530,
	EnhancedCode: smtp.EnhancedCode{5, 7, 0},
	Message:      "Authenticate first, with a Zulip bot email and API key",
}

func errNoSuchRecipient(to string) *smtp.SMTPError {
	return &smtp.SMTPError{
		Code:         550,
		EnhancedCode: smtp.EnhancedCode{5, 1, 1},
		Message:      fmt.Sprintf("%s is not a Zulip user in this organization", to),
	}
}

func errUnparseable(err error) *smtp.SMTPError {
	return &smtp.SMTPError{
		Code:         554,
		EnhancedCode: smtp.EnhancedCode{5, 6, 0},
		Message:      "Could not read the message: " + err.Error(),
	}
}
