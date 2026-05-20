package events

import (
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/nats-io/nats.go"

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
	// Drain flushes the internal buffer, signals subscribers (we have none),
	// then closes. Bounded by DrainTimeout (default 30s) — fine for shutdown.
	if err := p.nc.Drain(); err != nil {
		slog.Warn("kiosk.nats.drain_failed", "error", err)
	}
}

// Connect dials NATS using the auth knobs the operator filled in. Returns
// (nil, nil) when cfg.Enabled is false — main.go treats nil as a no-op
// publisher. Returns (nil, err) when enabled-but-unable-to-connect; that's
// a fatal startup condition (an operator misconfigured something).
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
	slog.Info("kiosk.nats.connected", "url", cfg.URL)
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
	opts = append(opts,
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
