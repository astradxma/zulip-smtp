package main

import (
	"io"
	"log/slog"
	"time"

	"github.com/emersion/go-sasl"
	"github.com/emersion/go-smtp"
)

type backend struct {
	zulip *zulipClient
}

func (b *backend) NewSession(c *smtp.Conn) (smtp.Session, error) {
	return &session{zulip: b.zulip}, nil
}

type session struct {
	zulip *zulipClient

	// Set once AUTH succeeds. These are the caller's own Zulip bot credentials;
	// they live for the length of one connection and are never written anywhere.
	botEmail string
	botKey   string

	rcpts []string
}

// AuthMechanisms advertises PLAIN only.
//
// LOGIN and CRAM-MD5 are deliberately absent: PLAIN is what every client
// speaks, and each extra mechanism is another code path for no gain. PLAIN is
// safe here for the same reason AllowInsecureAuth is -- see the note in main.go.
func (s *session) AuthMechanisms() []string {
	return []string{sasl.Plain}
}

// Auth validates the supplied credentials against Zulip itself.
//
// ★ This is error reporting, not a security control. The relay has no user
// database and grants nothing, so there is no local decision to make. What the
// check buys is that a caller with a bad key gets `535` at the AUTH step, where
// their client will report it, instead of a cheerful `250` followed by a message
// that silently never arrives.
//
// No caching. A cached "these credentials are good" would keep a revoked bot key
// working for the length of the TTL, and the volume here does not justify it.
func (s *session) Auth(mech string) (sasl.Server, error) {
	return sasl.NewPlainServer(func(identity, username, password string) error {
		if err := s.zulip.checkCredentials(username, password); err != nil {
			slog.Info("auth rejected", "bot", username, "err", err)
			return err
		}
		s.botEmail = username
		s.botKey = password
		return nil
	}), nil
}

// Mail accepts any sender and uses it for nothing.
//
// ★ The message is sent BY the authenticated bot, so MAIL FROM cannot influence
// who it appears to come from. Honouring it for authorization would be an
// invitation to spoof; ignoring it is both simpler and correct.
func (s *session) Mail(from string, opts *smtp.MailOptions) error {
	if s.botEmail == "" {
		return errAuthRequired
	}
	return nil
}

// Rcpt resolves the recipient to a Zulip user so a bad address fails at the
// step where it belongs, rather than after the whole body has been transferred.
func (s *session) Rcpt(to string, opts *smtp.RcptOptions) error {
	if s.botEmail == "" {
		return errAuthRequired
	}

	switch err := s.zulip.userExists(s.botEmail, s.botKey, to); {
	case err == nil:
		s.rcpts = append(s.rcpts, to)
		return nil

	case isNoSuchUser(err):
		slog.Info("recipient rejected", "bot", s.botEmail, "rcpt", to)
		return errNoSuchRecipient(to)

	default:
		// ★ Permissive on UNEXPECTED failures. If the lookup itself could not be
		// completed -- Zulip unreachable, an endpoint behaving differently than
		// we expect -- accept the recipient and let the send decide. Being strict
		// here would turn any change on Zulip's side into a total outage for a
		// check that is only a convenience.
		slog.Warn("recipient lookup inconclusive, accepting", "rcpt", to, "err", err)
		s.rcpts = append(s.rcpts, to)
		return nil
	}
}

func (s *session) Data(r io.Reader) error {
	if s.botEmail == "" {
		return errAuthRequired
	}

	started := time.Now()

	subject, body, n, err := extractMessage(r)
	if err != nil {
		slog.Info("unparseable message", "bot", s.botEmail, "bytes", n, "err", err)
		return errUnparseable(err)
	}

	content := formatForZulip(subject, body)

	// One DM per recipient, matching email semantics: each addressee gets their
	// own copy rather than being dropped into a group conversation with strangers.
	for _, to := range s.rcpts {
		if err := s.zulip.sendDirectMessage(s.botEmail, s.botKey, to, content); err != nil {
			slog.Info("delivery failed",
				"bot", s.botEmail, "rcpt", to, "bytes", n,
				"ms", time.Since(started).Milliseconds(), "err", err,
			)
			return err
		}
		slog.Info("delivered",
			"bot", s.botEmail, "rcpt", to, "bytes", n,
			"ms", time.Since(started).Milliseconds(),
		)
	}
	return nil
}

func (s *session) Reset() { s.rcpts = nil }

func (s *session) Logout() error { return nil }
