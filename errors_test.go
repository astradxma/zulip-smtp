package main

import (
	"net/http"
	"testing"
)

// The mapping table is the reliability design: a transient Zulip failure must
// come back 4xx so the sender retries. A 5xx here loses the message for good.
func TestZulipErrorToSMTP(t *testing.T) {
	cases := []struct {
		name      string
		err       zulipError
		wantCode  int
		transient bool
	}{
		{"bad api key", zulipError{status: http.StatusUnauthorized}, 535, false},
		{"unknown user", zulipError{status: http.StatusBadRequest, msg: "Invalid email"}, 550, false},
		{"not found", zulipError{status: http.StatusNotFound}, 550, false},
		{"rate limited by status", zulipError{status: http.StatusTooManyRequests}, 451, true},
		// A rate limit reported alongside a 4xx that would otherwise read as
		// permanent must still come back transient, or the message is dropped.
		{"rate limited by code", zulipError{status: 400, code: "RATE_LIMIT_HIT"}, 451, true},
		{"zulip down", zulipError{status: http.StatusBadGateway}, 451, true},
		{"network failure", zulipError{status: 0, msg: "dial tcp: timeout"}, 451, true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := c.err.toSMTP()
			if got.Code != c.wantCode {
				t.Errorf("code = %d, want %d", got.Code, c.wantCode)
			}
			isTransient := got.Code >= 400 && got.Code < 500
			if isTransient != c.transient {
				t.Errorf("transient = %v, want %v (code %d)", isTransient, c.transient, got.Code)
			}
		})
	}
}

func TestIsNoSuchUser(t *testing.T) {
	if !isNoSuchUser(&zulipError{status: http.StatusNotFound}) {
		t.Error("404 should read as no such user")
	}
	if isNoSuchUser(&zulipError{status: http.StatusTooManyRequests}) {
		t.Error("429 is not a statement about the recipient")
	}
	if isNoSuchUser(nil) {
		t.Error("nil is not an error")
	}
}
