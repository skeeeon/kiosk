// Package rfid wraps an LLRP RFID reader (e.g. an Impinj R700) behind
// a small domain interface the rest of the kiosk binary can mock for
// tests. The underlying LLRP protocol implementation comes from
// EdgeX's device-rfid-llrp-go/pkg/llrp; that import is an
// implementation detail of impinjReader and is not re-exported.
//
// Phase 1 lands the package shape, connection lifecycle, and a stub
// ReadFor. The LLRP inventory-cycle message dance (AddROSpec /
// EnableROSpec / ROAccessReport collection / DisableROSpec /
// DeleteROSpec) lands in Phase 2 alongside the first caller. See
// docs/rfid.md for the full design.
package rfid

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/edgexfoundry/device-rfid-llrp-go/pkg/llrp"

	"github.com/skeeeon/kiosk/internal/config"
)

// EPC is a tag's Electronic Product Code as a hex string (no 0x prefix,
// upper or lower case as the reader emits it). The kiosk treats EPCs
// opaquely; resolution to item_instances happens in scan.Resolver via
// the existing rfid_epc field.
type EPC string

// Reader is the wrapper interface handlers and command dispatchers use.
// Tests substitute a fake; production code uses impinjReader (created
// via New).
type Reader interface {
	// Connect dials the reader and starts the LLRP client. Returns
	// quickly even if the LLRP handshake is still in flight — sends
	// queue until the client is ready. Errors here are dial-level
	// (host unreachable, refused, etc.) and should be treated as
	// best-effort by the caller: a kiosk with no reader should still
	// boot.
	Connect(ctx context.Context) error

	// ReadFor runs a single inventory cycle for the given duration and
	// returns the set of observed EPCs. Phase 1 returns
	// ErrReadForNotImplemented; Phase 2 lands the real impl.
	ReadFor(ctx context.Context, d time.Duration) ([]EPC, error)

	// Close shuts the LLRP client down cleanly. Safe to call when
	// Connect was never called or already failed — in that case it's
	// a no-op.
	Close() error
}

// ErrReadForNotImplemented is what the Phase 1 stub returns. Callers
// that want to detect "the wrapper is dormant" check via errors.Is.
var ErrReadForNotImplemented = errors.New("rfid: ReadFor not implemented in phase 1")

// dialTimeout caps how long we'll wait for the initial TCP handshake.
// The LLRP-level handshake that follows is asynchronous and not bounded
// here — sends queue against the client until it's ready.
const dialTimeout = 5 * time.Second

// shutdownTimeout caps how long Close will wait for the graceful LLRP
// CloseConnection round-trip before falling back to a hard Close.
const shutdownTimeout = 2 * time.Second

// New returns a Reader configured from the kiosk's RFID config block.
// It does not dial yet — call Connect for that. Returns an error only
// for static config problems the config package's own validation
// should have already caught; in practice this is a defense-in-depth
// check.
func New(cfg config.RFIDConfig) (Reader, error) {
	if !cfg.Enabled {
		return nil, errors.New("rfid: New called with enabled=false")
	}
	if cfg.Reader.Host == "" || cfg.Reader.Port == 0 {
		return nil, errors.New("rfid: reader host/port not configured")
	}
	return &impinjReader{
		addr: net.JoinHostPort(cfg.Reader.Host, strconv.Itoa(cfg.Reader.Port)),
	}, nil
}

// impinjReader is the production Reader. The naming reflects that we
// only test against Impinj R-series hardware in v1 — the wire protocol
// is LLRP-standard, but vendor quirks (Impinj custom messages,
// antenna power tuning, etc.) belong here when we add them. The
// underlying *llrp.Client is vendor-agnostic.
type impinjReader struct {
	addr string

	mu     sync.Mutex
	client *llrp.Client
	conn   net.Conn
	done   chan error // signals when client.Connect's goroutine exits
}

// Connect dials the reader and starts the LLRP client goroutine. The
// goroutine runs client.Connect(conn) which blocks until the
// connection drops or Close is called — its return value lands on
// r.done for inspection by Close.
//
// We do not wait for the LLRP handshake to complete here. The LLRP
// client queues sends until it's ready, so a ReadFor that arrives
// before the handshake finishes will simply block briefly. Phase 1
// has no caller that exercises this; Phase 2 will need to revisit if
// the queuing semantics turn out to surprise us.
func (r *impinjReader) Connect(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.client != nil {
		return errors.New("rfid: already connected")
	}

	dialCtx, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()

	conn, err := (&net.Dialer{}).DialContext(dialCtx, "tcp", r.addr)
	if err != nil {
		return fmt.Errorf("rfid: dial %s: %w", r.addr, err)
	}

	client := llrp.NewClient()
	done := make(chan error, 1)

	go func() {
		// client.Connect blocks until the connection drops or Close
		// fires. Normal shutdown returns llrp.ErrClientClosed which we
		// translate to a nil signal so Close sees "exited cleanly".
		err := client.Connect(conn)
		if errors.Is(err, llrp.ErrClientClosed) {
			err = nil
		}
		done <- err
	}()

	r.client = client
	r.conn = conn
	r.done = done
	return nil
}

func (r *impinjReader) ReadFor(ctx context.Context, d time.Duration) ([]EPC, error) {
	return nil, ErrReadForNotImplemented
}

// Close shuts the client down. Calls Shutdown first for a graceful
// CloseConnection round-trip; if that fails or times out, falls back
// to a hard Close. Either path triggers the goroutine started in
// Connect to return, which we briefly wait for so the caller knows the
// reader is fully torn down.
func (r *impinjReader) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.client == nil {
		return nil
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	// Shutdown's error is best-effort — if the reader is already gone
	// the graceful exchange will fail and we still want to hard-close.
	_ = r.client.Shutdown(shutdownCtx)
	_ = r.client.Close()

	// Connect's goroutine exits once Close fires; give it a moment so
	// the caller sees a fully drained Reader. The timeout here is
	// generous because we just want to avoid a leaked goroutine, not
	// to wait for slow network teardown.
	select {
	case <-r.done:
	case <-time.After(shutdownTimeout):
	}

	if r.conn != nil {
		_ = r.conn.Close()
	}

	r.client = nil
	r.conn = nil
	r.done = nil
	return nil
}
