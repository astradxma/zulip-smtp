// Command zulip-smtp is an SMTP relay that turns mail into Zulip direct messages.
//
// It exists because almost everything that sends a password-reset email speaks
// SMTP and nothing speaks Zulip. Keycloak, Grafana, Bareos and friends all have
// a four-field mail config box; none of them will grow a Zulip integration.
//
// ★ It is stateless and holds no credentials. The caller's Zulip bot email and
// API key arrive in SMTP AUTH and are used for exactly one API call. The only
// thing this process knows is which Zulip server to talk to -- and that is here
// purely so callers do not have to encode it into an address.
//
// The security posture follows from that: anyone holding valid Zulip bot
// credentials could send the same message with curl, so the relay confers no
// privilege and implements no authorization of its own.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/emersion/go-smtp"
)

// version is stamped by GoReleaser via -ldflags; "dev" for a plain `go build`.
var version = "dev"

type config struct {
	zulipSite       string
	listenAddr      string
	healthAddr      string
	smtpDomain      string
	maxMessageBytes int64
	maxRecipients   int
	readTimeout     time.Duration
	writeTimeout    time.Duration
}

func loadConfig() (config, error) {
	c := config{
		zulipSite:       os.Getenv("ZULIP_SITE"),
		listenAddr:      envOr("LISTEN_ADDR", ":1025"),
		healthAddr:      envOr("HEALTH_ADDR", ":8080"),
		smtpDomain:      envOr("SMTP_DOMAIN", "zulipsmtp.internal.astradx.com"),
		maxMessageBytes: int64(envIntOr("MAX_MESSAGE_BYTES", 1<<20)),
		maxRecipients:   envIntOr("MAX_RECIPIENTS", 10),
		readTimeout:     time.Duration(envIntOr("READ_TIMEOUT_SECONDS", 60)) * time.Second,
		writeTimeout:    time.Duration(envIntOr("WRITE_TIMEOUT_SECONDS", 60)) * time.Second,
	}
	if c.zulipSite == "" {
		return c, errors.New("ZULIP_SITE is required (e.g. https://astradx.zulipchat.com)")
	}
	return c, nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envIntOr(key string, fallback int) int {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		slog.Warn("ignoring unparseable env var", "key", key, "fallback", fallback)
		return fallback
	}
	return n
}

// runHealthCheck probes the local health endpoint and exits.
//
// The image is distroless -- no shell, no curl -- so the binary has to be its
// own health check. `zulip-smtp -healthcheck` is what the container HEALTHCHECK
// runs; it talks to this same process over loopback.
func runHealthCheck(addr string) {
	if strings.HasPrefix(addr, ":") {
		addr = "127.0.0.1" + addr
	}
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get("http://" + addr + "/healthz")
	if err != nil {
		fmt.Fprintln(os.Stderr, "healthcheck:", err)
		os.Exit(1)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintln(os.Stderr, "healthcheck: status", resp.StatusCode)
		os.Exit(1)
	}
	os.Exit(0)
}

func main() {
	healthcheck := flag.Bool("healthcheck", false, "probe the local health endpoint and exit")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println("zulip-smtp", version)
		return
	}
	if *healthcheck {
		runHealthCheck(envOr("HEALTH_ADDR", ":8080"))
		return
	}

	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	})))

	cfg, err := loadConfig()
	if err != nil {
		slog.Error("bad configuration", "err", err)
		os.Exit(1)
	}

	zc := newZulipClient(cfg.zulipSite)

	srv := smtp.NewServer(&backend{zulip: zc})
	srv.Addr = cfg.listenAddr
	srv.Domain = cfg.smtpDomain
	srv.ReadTimeout = cfg.readTimeout
	srv.WriteTimeout = cfg.writeTimeout
	srv.MaxMessageBytes = cfg.maxMessageBytes
	srv.MaxRecipients = cfg.maxRecipients

	// ⚠️ THIS IS CORRECT HERE. DO NOT "FIX" IT.
	//
	// AllowInsecureAuth permits AUTH on a connection this process sees as
	// plaintext -- which in most deployments would mean handing out everyone's
	// credentials. It is right here because TLS was terminated one hop earlier,
	// by Traefik, on :465. Callers get a real Let's Encrypt certificate; only
	// the Traefik-to-here hop is bare, and it does not leave the host.
	//
	// What actually protects that hop: this container publishes NO host port,
	// so nothing on the LAN or the tailnet can reach :1025. It is reachable from
	// the `traefik` docker network -- i.e. from other infrastructure containers
	// on this box, which is the same trust boundary every other routed service
	// on woodpecker already sits inside.
	//
	// So the deployment IS the enforcement. If this ever gains a `ports:` entry,
	// that stops being true and this line becomes the hole it looks like.
	srv.AllowInsecureAuth = true

	// Liveness only, on its own port. Never routed, never published -- it exists
	// so `docker compose` and Komodo can tell a wedged process from a live one,
	// because SMTP has no natural health endpoint.
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, "ok")
	})
	health := &http.Server{
		Addr:              cfg.healthAddr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		if err := health.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("health listener stopped", "err", err)
		}
	}()

	go func() {
		slog.Info("listening",
			"version", version,
			"smtp", cfg.listenAddr,
			"health", cfg.healthAddr,
			"zulip", cfg.zulipSite,
			"domain", cfg.smtpDomain,
		)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, smtp.ErrServerClosed) {
			slog.Error("smtp listener stopped", "err", err)
			os.Exit(1)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop

	slog.Info("shutting down")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = health.Shutdown(ctx)
	_ = srv.Shutdown(ctx)
}
