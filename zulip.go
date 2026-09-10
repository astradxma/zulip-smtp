package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// zulipClient talks to one Zulip server. It holds no credentials of its own --
// every call takes the caller's bot email and API key, used as HTTP basic auth.
type zulipClient struct {
	site string // e.g. https://astradx.zulipchat.com  (no trailing slash)
	http *http.Client
}

func newZulipClient(site string) *zulipClient {
	return &zulipClient{
		site: strings.TrimSuffix(site, "/"),
		http: &http.Client{Timeout: 20 * time.Second},
	}
}

// apiResponse is the envelope every Zulip endpoint returns.
type apiResponse struct {
	Result string `json:"result"`
	Msg    string `json:"msg"`
	Code   string `json:"code"`
}

func (c *zulipClient) do(req *http.Request, botEmail, botKey string) error {
	req.SetBasicAuth(botEmail, botKey)
	req.Header.Set("User-Agent", "zulip-smtp")

	resp, err := c.http.Do(req)
	if err != nil {
		// Network-level failure: transient by definition.
		return &zulipError{status: 0, msg: err.Error()}
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))

	var parsed apiResponse
	_ = json.Unmarshal(body, &parsed)

	if resp.StatusCode == http.StatusOK && parsed.Result == "success" {
		return nil
	}

	msg := parsed.Msg
	if msg == "" {
		msg = strings.TrimSpace(string(body))
		if len(msg) > 200 {
			msg = msg[:200]
		}
	}
	return &zulipError{status: resp.StatusCode, code: parsed.Code, msg: msg}
}

// checkCredentials verifies a bot email and API key are accepted by Zulip.
func (c *zulipClient) checkCredentials(botEmail, botKey string) error {
	req, err := http.NewRequest(http.MethodGet, c.site+"/api/v1/users/me", nil)
	if err != nil {
		return &zulipError{msg: err.Error()}
	}
	if err := c.do(req, botEmail, botKey); err != nil {
		if ze, ok := err.(*zulipError); ok {
			return ze.toSMTP()
		}
		return err
	}
	return nil
}

// userExists resolves an email address to a Zulip user in this organization.
//
// Returns a *zulipError so the caller can distinguish "Zulip says no such user"
// from "the lookup could not be completed" -- those get different treatment at
// RCPT TO, see session.Rcpt.
func (c *zulipClient) userExists(botEmail, botKey, target string) error {
	endpoint := c.site + "/api/v1/users/" + url.PathEscape(target)
	req, err := http.NewRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		return &zulipError{msg: err.Error()}
	}
	return c.do(req, botEmail, botKey)
}

// sendDirectMessage posts one DM, as the authenticated bot.
func (c *zulipClient) sendDirectMessage(botEmail, botKey, target, content string) error {
	recipients, err := json.Marshal([]string{target})
	if err != nil {
		return &zulipError{msg: err.Error()}
	}

	form := url.Values{}
	form.Set("type", "direct")
	form.Set("to", string(recipients))
	form.Set("content", content)

	req, reqErr := http.NewRequest(http.MethodPost,
		c.site+"/api/v1/messages", strings.NewReader(form.Encode()))
	if reqErr != nil {
		return &zulipError{msg: reqErr.Error()}
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	if err := c.do(req, botEmail, botKey); err != nil {
		if ze, ok := err.(*zulipError); ok {
			return ze.toSMTP()
		}
		return fmt.Errorf("sending to %s: %w", target, err)
	}
	return nil
}
