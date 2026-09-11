# zulip-smtp

An SMTP relay that turns mail into **Zulip direct messages**.

It exists because nearly everything that sends a password-reset email speaks
SMTP, and nothing speaks Zulip. Keycloak, Grafana, Bareos and friends all have a
four-field mail config box; none of them is going to grow a Zulip integration.
So rather than a bridge per service, one address the whole company can point at.

```
any app ──SMTP over TLS :465──▶ Traefik ──plain SMTP :1025──▶ zulip-smtp ──HTTPS──▶ Zulip DM
```

## The posture, stated once

**This service holds no credentials and grants no privilege.**

The caller's Zulip bot email and API key arrive in SMTP `AUTH` and are used for
exactly one API call. The only thing this process knows is which Zulip server to
talk to — and that is here purely so callers do not have to encode it into an
address.

Which means the relay is not a security boundary and does not try to be: anyone
holding valid Zulip bot credentials could send the same message with `curl`.
There is no user database, no allowlist, no per-team configuration.

The one thing that *is* true and worth respecting: credentials transit this
process. That makes two rules load-bearing rather than merely tidy —
**the credential is never logged**, and **TLS on the caller's hop is not
optional**. Without the second, this would quietly collect the company's bot
keys off the wire.

## Configuring a sender

Four fields, in whatever mail settings screen the app already has:

| Field | Value |
| -- | -- |
| Host | `zulipsmtp.internal.astradx.com` |
| Port | `465` |
| SSL / TLS | **on** (implicit TLS — the label often still says "SSL") |
| Auth | **on** |
| Username | the Zulip bot's email, e.g. `keycloak-bot@astradx.zulipchat.com` |
| Password | that bot's Zulip API key |

`From:` can be anything; it is ignored. The message is sent **by the
authenticated bot**, so the envelope sender cannot influence how it appears —
honouring it would only be an invitation to spoof.

Recipients are Zulip users, addressed by the email they use in Zulip. Each
`RCPT TO` gets its own DM, matching email semantics rather than dropping several
people into a group conversation.

## What it does with a message

* Prefers `text/plain`. Falls back to `text/html` converted to text.
* Subject becomes a bold first line.
* **Links in HTML-only mail are preserved.** A reset mail whose only content is
  `<a href="…token">Reset</a>` would otherwise arrive as the word "Reset" and be
  useless — it looks fine and does nothing. See `htmlToText`.
* Truncates at Zulip's 10,000-character limit, visibly.
* **Attachments are ignored** — deliberately, and said out loud rather than
  silently dropped.

## Error codes, and why they are the design

SMTP splits failures into permanent (`5xx` — the client gives up) and transient
(`4xx` — the client retries). Getting this wrong is how a brief Zulip hiccup
silently loses somebody's password reset.

| Zulip says | Relay returns | |
| -- | -- | -- |
| 401 unauthorized | `535` | permanent — the key is wrong |
| 404 / 400 no such user | `550` | permanent — not a Zulip user here |
| 429 or `RATE_LIMIT_HIT` | `451` | **transient — retry** |
| 5xx, timeout, unreachable | `451` | transient |

`errors.go` tests rate limiting **before** any status-based rule, on purpose.
Zulip sends 429 today, but a rate limit arriving with some other 4xx must not
fall through to the permanent `550` branch. Do not sort those cases for
tidiness — there is a test that will fail if you do.

## Logging

Structured JSON: bot, recipient, byte count, result, duration.

**Never** the subject, the body, or the API key. This carries other people's
password-reset links; a log line is not the place for them. (The `keycloak-zulip-bridge`
proof-of-concept logged subjects — fine for one service you own, wrong here.)

## Installing the binary

Every tagged release publishes statically linked binaries for linux and macOS,
amd64 and arm64, as `.tar.gz` with a `checksums.txt` — the same shape as
lazygit and most small Go tools.

```sh
curl -fsSL https://github.com/astradxma/zulip-smtp/releases/latest/download/zulip-smtp_$(uname -s | tr A-Z a-z)_$(uname -m | sed 's/x86_64/amd64/;s/aarch64/arm64/').tar.gz \
  | tar xz zulip-smtp
./zulip-smtp -version
```

(Substitute the version in place of `latest` for a pinned install; asset names
are `zulip-smtp_<version>_<os>_<arch>.tar.gz`.)

## Deploying

The image is public at `ghcr.io/astradxma/zulip-smtp`, multi-arch, built by
GitHub-hosted runners on every tag — nothing in the release path touches the
tailnet or magpie. `Dockerfile` is for local development builds only.

Prerequisite, once, in `adx-servers/trafeik-woodpecker/docker-compose.yml`:

```yaml
- "--entrypoints.smtps.address=:465"
```

Then:

```sh
TAG=0.1.0 docker compose up -d
```

⚠️ **There is no `ports:` block and there must never be one.** Traefik terminates
TLS and forwards plain SMTP here; this process accepts `AUTH` on a connection it
sees as plaintext. That is correct while Traefik is the only way in, and becomes
exactly the hole it looks like the moment a host port is published.

### Why `HostSNI(*)` rather than the hostname

A TCP router matching on a name needs the client to send SNI, and SMTP clients
are inconsistent about it — one that omits it silently fails to match and the
connection dies with nothing useful in any log. Port 465 serves exactly one
service, so there is nothing to discriminate between; `tls.domains` names the
certificate. Same reasoning as the NATS router in `elephant-events`.

### Why 465 and not 587

**Traefik cannot terminate STARTTLS.** On 587 the connection opens in plaintext
and upgrades mid-conversation in a language Traefik does not speak, so the
certificate would have to live in this container along with renewal and reload.
465 is implicit TLS — the envelope starts at the first byte, Traefik handles it,
and this process never sees a certificate.

## Verifying

```sh
openssl s_client -connect zulipsmtp.internal.astradx.com:465 \
                 -servername zulipsmtp.internal.astradx.com
```

Expect the Let's Encrypt chain, then a `220` banner. That the banner arrives
*after* the handshake rather than before is the proof implicit TLS is working.
Then `EHLO x` should list `250-AUTH PLAIN`.

## Development

```sh
go test ./...
ZULIP_SITE=https://astradx.zulipchat.com go run .   # listens on :1025 plaintext
```

To cut a release, push a tag; `release.yml` does the rest:

```sh
git tag v0.1.0 && git push origin v0.1.0
```

To see what a release would produce without publishing anything:

```sh
go run github.com/goreleaser/goreleaser/v2@latest release --snapshot --clean --skip=publish
ls dist/
```

Locally there is no Traefik, so connect without TLS on 1025 and authenticate
with a real bot's credentials.
