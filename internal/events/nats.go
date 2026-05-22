package events

import (
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/skeeeon/kiosk/internal/config"
)

// Publisher is the interface SetPublisher accepts. The production
// implementation wraps a nats.Conn; tests inject their own to capture
// calls. Kept tiny on purpose — we only publish, we never consume.
type Publisher interface {
	PublishJSON(subject string, payload any) error
	Close()
}

// natsPublisher is the production implementation. It marshals each payload
// once and hands it to nats.Conn.Publish, which buffers internally. Errors
// are returned to the caller, which logs them; we don't retry — the NATS
// client's own reconnect logic handles transient disconnects.
type natsPublisher struct {
	nc *nats.Conn
}

func (p *natsPublisher) PublishJSON(subject string, payload any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal payload: %w", err)
	}
	return p.nc.Publish(subject, data)
}

func (p *natsPublisher) Close() {
	if p == nil || p.nc == nil {
		return
	}
	// Drain only works on a connected conn; if we're still mid-reconnect
	// (e.g., never reached the server since startup), Drain errors. In that
	// case just close — there's nothing usefully drainable.
	if p.nc.IsConnected() {
		if err := p.nc.Drain(); err != nil {
			slog.Warn("kiosk.nats.drain_failed", "error", err)
		}
		return
	}
	p.nc.Close()
}

// Conn exposes the underlying *nats.Conn for callers that need JetStream,
// KV, or a consumer (e.g., the kiosk's catalog watcher and the controller).
// Returning the live conn is safe because both Conn() and PublishJSON share
// it — there's no parallel state to coordinate.
func (p *natsPublisher) Conn() *nats.Conn { return p.nc }

// JetStream returns a jetstream.JetStream context from the active publisher.
// Errors when the publisher is nil/disabled (NATS not enabled) or when it's
// a non-NATS test fake. Callers that need JS during startup should treat a
// nil err + nil JS as "JS not available, run in degraded mode."
func JetStream(p Publisher) (jetstream.JetStream, error) {
	if p == nil {
		return nil, errors.New("nats publisher is nil — events.enabled likely false")
	}
	type connHolder interface{ Conn() *nats.Conn }
	holder, ok := p.(connHolder)
	if !ok {
		return nil, errors.New("publisher does not expose a NATS connection")
	}
	nc := holder.Conn()
	if nc == nil {
		return nil, errors.New("publisher has no NATS connection")
	}
	return jetstream.New(nc)
}

// Connect dials NATS using the auth knobs the operator filled in. Returns
// (nil, nil) when cfg.Enabled is false — main.go treats nil as a no-op
// publisher.
//
// Network unreachability is NOT an error: with RetryOnFailedConnect, the
// returned *nats.Conn enters a connecting/buffering state and dials in
// the background. The kiosk's primary job is local checkout against the
// ledger; NATS is a best-effort feed to the central reporting consumer
// and should never block the kiosk from starting. An (nil, err) return
// here means a structural config problem (empty URL, malformed URL,
// unloadable creds) that won't fix itself by waiting.
func Connect(cfg config.NATSConfig) (Publisher, error) {
	if !cfg.Enabled {
		return nil, nil
	}
	if cfg.URL == "" {
		return nil, errors.New("nats.enabled=true but nats.url is empty")
	}

	opts := buildNATSOptions(cfg)
	nc, err := nats.Connect(cfg.URL, opts...)
	if err != nil {
		return nil, fmt.Errorf("connect %s: %w", cfg.URL, err)
	}
	if nc.IsConnected() {
		slog.Info("kiosk.nats.connected", "url", cfg.URL)
	} else {
		slog.Warn("kiosk.nats.connect_deferred",
			"url", cfg.URL,
			"note", "events will buffer locally until the server is reachable")
	}
	return &natsPublisher{nc: nc}, nil
}

// buildNATSOptions translates the config struct into nats.Options. Auth
// knobs compose so an operator can mix (e.g., TLS + nkey).
func buildNATSOptions(cfg config.NATSConfig) []nats.Option {
	var opts []nats.Option

	// Identify ourselves for nats-server logs.
	opts = append(opts, nats.Name("kiosk"))

	// Reconnect aggressively but quietly — kiosks live in flaky shop
	// networks and we'd rather buffer-and-retry than fail-fast.
	//
	// RetryOnFailedConnect makes the *initial* dial behave the same way:
	// if the server isn't reachable at startup, nats.Connect still returns
	// a valid connection that buffers publishes and dials in the background.
	// This is what lets the kiosk boot even when the central NATS endpoint
	// is down — the local ledger is authoritative regardless.
	opts = append(opts,
		nats.RetryOnFailedConnect(true),
		nats.ReconnectWait(2*time.Second),
		nats.MaxReconnects(-1), // forever
		nats.DisconnectErrHandler(func(_ *nats.Conn, err error) {
			if err != nil {
				slog.Warn("kiosk.nats.disconnected", "error", err)
			}
		}),
		nats.ReconnectHandler(func(nc *nats.Conn) {
			slog.Info("kiosk.nats.reconnected", "url", nc.ConnectedUrl())
		}),
		nats.ErrorHandler(func(_ *nats.Conn, _ *nats.Subscription, err error) {
			slog.Warn("kiosk.nats.async_error", "error", err)
		}),
	)

	// Auth modes — apply each that's configured. nats.go is happy with
	// multiple options; the server enforces what it actually requires.
	if cfg.Token != "" {
		opts = append(opts, nats.Token(cfg.Token))
	}
	if cfg.Username != "" || cfg.Password != "" {
		opts = append(opts, nats.UserInfo(cfg.Username, cfg.Password))
	}
	if cfg.CredentialsFile != "" {
		opts = append(opts, nats.UserCredentials(cfg.CredentialsFile))
	}
	if cfg.NKeySeedFile != "" {
		if opt, err := nats.NkeyOptionFromSeed(cfg.NKeySeedFile); err == nil {
			opts = append(opts, opt)
		} else {
			slog.Warn("kiosk.nats.nkey_load_failed", "path", cfg.NKeySeedFile, "error", err)
		}
	}

	// TLS — explicit knobs override the URL scheme's default.
	if cfg.TLSCAFile != "" {
		opts = append(opts, nats.RootCAs(cfg.TLSCAFile))
	}
	if cfg.TLSCertFile != "" && cfg.TLSKeyFile != "" {
		opts = append(opts, nats.ClientCert(cfg.TLSCertFile, cfg.TLSKeyFile))
	}
	if cfg.TLSInsecure {
		opts = append(opts, nats.Secure(&tls.Config{InsecureSkipVerify: true})) //nolint:gosec
	}

	return opts
}
