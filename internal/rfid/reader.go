// Package rfid wraps an LLRP RFID reader (e.g. an Impinj R700) behind
// a small domain interface the rest of the kiosk binary can mock for
// tests. The underlying LLRP protocol implementation comes from
// EdgeX's device-rfid-llrp-go/pkg/llrp; that import is an
// implementation detail of impinjReader and is not re-exported.
//
// Phase 2 implements ReadFor — one LLRP inventory cycle per call —
// against the live reader: AddROSpec / EnableROSpec / collect
// ROAccessReport / DisableROSpec / DeleteROSpec. See docs/rfid.md
// for the full design.
package rfid

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
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

// ErrNotConnected is returned by ReadFor when Connect hasn't run or
// the connection was torn down. Callers can branch on this if they
// want different behavior from generic LLRP errors (e.g. a kiosk that
// boots without the reader plugged in but should still serve manual
// scans).
var ErrNotConnected = errors.New("rfid: not connected")

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

	// readMu serializes ReadFor calls so we never have two ROSpecs
	// fighting over the same ROSpecID, and so the ROAccessReport
	// handler always has an unambiguous owner. One read at a time
	// matches reality (one operator at a kiosk; one cabinet door
	// open at a time in enclosure_diff) — contention is not a
	// concern, correctness is.
	readMu sync.Mutex

	// accumMu guards `accum` between the LLRP read-side goroutine
	// (where the ROAccessReport handler fires) and the ReadFor
	// goroutine that drains it. Stored as a pointer so ReadFor can
	// swap in a fresh slice atomically — old reads landing late from
	// the previous cycle don't pollute the next one.
	accumMu sync.Mutex
	accum   *[]EPC
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

	// The ROAccessReport handler is the only thing the read-side
	// goroutine ever pushes into. It must not block — the LLRP client
	// runs handlers inline with the read loop, so a stalled handler
	// stalls the whole connection. We do a cheap unmarshal + mutex'd
	// append and return.
	client := llrp.NewClient(
		llrp.WithMessageHandler(llrp.MsgROAccessReport,
			llrp.MessageHandlerFunc(r.onROAccessReport)),
	)
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

// ReadFor runs one LLRP inventory cycle for d and returns the
// deduplicated EPCs observed. The flow:
//
//  1. Reset the in-impinjReader accumulator to a fresh slice.
//  2. Send AddROSpec; check LLRPStatus.
//  3. Send EnableROSpec; check LLRPStatus. The ROSpec auto-starts
//     (ROStartTriggerImmediate) and auto-stops after d ms
//     (ROStopTriggerDuration). ROAccessReport messages flow inbound
//     during that window and the handler accumulates EPCs.
//  4. Wait for d, or for ctx to cancel — whichever first.
//  5. Send DisableROSpec + DeleteROSpec, best-effort, on a fresh
//     short-timeout ctx so cleanup happens even when the caller's ctx
//     is already dead.
//
// One ROSpec ID is used for all calls (ROSpecID 1). We delete on the
// way out so the next call's Add is clean. Concurrent ReadFor calls
// serialize on readMu; this matches reality (one operator, one
// door).
func (r *impinjReader) ReadFor(ctx context.Context, d time.Duration) ([]EPC, error) {
	r.readMu.Lock()
	defer r.readMu.Unlock()

	r.mu.Lock()
	client := r.client
	r.mu.Unlock()
	if client == nil {
		return nil, ErrNotConnected
	}

	// Fresh accumulator. Any stragglers from a previous (cancelled)
	// read drop on the floor.
	bucket := make([]EPC, 0, 16)
	r.accumMu.Lock()
	r.accum = &bucket
	r.accumMu.Unlock()
	defer func() {
		r.accumMu.Lock()
		r.accum = nil
		r.accumMu.Unlock()
	}()

	spec := buildROSpec(d)

	// Add + Enable. Failures on either short-circuit; cleanup is
	// best-effort but we still try, because the reader may have
	// accepted Add even if Enable failed.
	if err := sendAndCheck(ctx, client, spec.Add(), &llrp.AddROSpecResponse{}); err != nil {
		r.cleanupSpec(spec) // no-op if Add never landed
		return nil, fmt.Errorf("rfid: AddROSpec: %w", err)
	}
	if err := sendAndCheck(ctx, client, spec.Enable(), &llrp.EnableROSpecResponse{}); err != nil {
		r.cleanupSpec(spec)
		return nil, fmt.Errorf("rfid: EnableROSpec: %w", err)
	}

	// Wait for the read window or ctx cancel. The ROSpec stops itself
	// on the reader side after d ms, but we still send Disable below
	// so a torn read leaves clean state for next time.
	select {
	case <-time.After(d):
	case <-ctx.Done():
	}

	r.cleanupSpec(spec)

	// Snapshot + deduplicate the accumulator. ROAccessReport handlers
	// can fire briefly after Disable as in-flight messages drain
	// through; the readMu we still hold prevents the next ReadFor from
	// stealing them, and the defer above clears r.accum after the
	// snapshot.
	r.accumMu.Lock()
	raw := append([]EPC(nil), bucket...)
	r.accumMu.Unlock()

	return dedupEPCs(raw), nil
}

// cleanupSpec sends Disable + Delete on a short, independent context
// so it runs even when the caller's ctx is dead. Errors are logged
// implicitly via the LLRP client's logger — we don't surface them
// because the caller already knows the read window completed; whether
// the reader's state is perfectly clean is best-effort.
func (r *impinjReader) cleanupSpec(spec *llrp.ROSpec) {
	cleanupCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = sendAndCheck(cleanupCtx, r.client, spec.Disable(), &llrp.DisableROSpecResponse{})
	_ = sendAndCheck(cleanupCtx, r.client, spec.Delete(), &llrp.DeleteROSpecResponse{})
}

// onROAccessReport is wired in via WithMessageHandler at NewClient
// time. It runs on the LLRP client's read goroutine — keep the work
// here cheap. We decode the report, extract EPCs from each tag, and
// append into whatever accumulator ReadFor has installed. When no
// ReadFor is in flight, r.accum is nil and the tags are silently
// dropped (the reader shouldn't be sending unsolicited reports
// outside a ROSpec, but if it does we don't want to crash).
func (r *impinjReader) onROAccessReport(_ *llrp.Client, msg llrp.Message) {
	rpt := &llrp.ROAccessReport{}
	if err := msg.UnmarshalTo(rpt); err != nil {
		return
	}
	if len(rpt.TagReportData) == 0 {
		return
	}

	r.accumMu.Lock()
	defer r.accumMu.Unlock()
	if r.accum == nil {
		return
	}
	for _, tag := range rpt.TagReportData {
		if e := epcFromTag(tag); e != "" {
			*r.accum = append(*r.accum, e)
		}
	}
}

// epcFromTag prefers EPC96 when populated; otherwise falls back to
// EPCData. Hex-encoded lower-case is what the reader emits in our
// downstream representations and matches the existing rfid_epc field
// format on item_instances (callers handle case-insensitive compare).
func epcFromTag(tag llrp.TagReportData) EPC {
	switch {
	case len(tag.EPC96.EPC) > 0:
		return EPC(strings.ToLower(hex.EncodeToString(tag.EPC96.EPC)))
	case len(tag.EPCData.EPC) > 0:
		return EPC(strings.ToLower(hex.EncodeToString(tag.EPCData.EPC)))
	}
	return ""
}

// dedupEPCs collapses repeated observations of the same tag (a
// 3-second read window typically sees each tag many times) into a
// single entry while preserving first-seen order.
func dedupEPCs(in []EPC) []EPC {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[EPC]struct{}, len(in))
	out := make([]EPC, 0, len(in))
	for _, e := range in {
		if _, ok := seen[e]; ok {
			continue
		}
		seen[e] = struct{}{}
		out = append(out, e)
	}
	return out
}

// sendAndCheck sends an LLRP request and asserts the response's
// LLRPStatus is success. The body is whichever Statusable response
// type the caller passes — we don't care about the rest of the
// payload at this layer.
func sendAndCheck(ctx context.Context, c *llrp.Client, out llrp.Outgoing, in llrp.Incoming) error {
	if err := c.SendFor(ctx, out, in); err != nil {
		return err
	}
	if s, ok := in.(llrp.Statusable); ok {
		st := s.Status()
		if st.Status != llrp.StatusSuccess {
			return fmt.Errorf("LLRP status %d: %s", st.Status, st.ErrorDescription)
		}
	}
	return nil
}

// rfidROSpecID is the single ROSpec slot we (re)use for every
// counter_scan / enclosure_diff cycle. Phase 4 may pick a different
// ID for enclosure_diff if it ever wants concurrent reads — for now
// one slot is enough since readMu serializes everything.
const rfidROSpecID = 1

// buildROSpec returns a minimal ROSpec configured to start
// immediately, run for d, deliver results when the RO ends, and
// inventory all antennas using EPC Gen2. Antenna power, channel
// selection, and Impinj custom extensions are intentionally
// left to the reader's own configuration — the kiosk doesn't tune
// from here.
func buildROSpec(d time.Duration) *llrp.ROSpec {
	ms := uint32(d / time.Millisecond)
	if ms == 0 {
		ms = 1 // 0 would mean "never stop"; clamp defensively
	}
	return &llrp.ROSpec{
		ROSpecID:           rfidROSpecID,
		Priority:           0,
		ROSpecCurrentState: llrp.ROSpecStateDisabled,
		ROBoundarySpec: llrp.ROBoundarySpec{
			StartTrigger: llrp.ROSpecStartTrigger{
				Trigger: llrp.ROStartTriggerImmediate,
			},
			StopTrigger: llrp.ROSpecStopTrigger{
				Trigger:              llrp.ROStopTriggerDuration,
				DurationTriggerValue: llrp.Millisecs32(ms),
			},
		},
		AISpecs: []llrp.AISpec{{
			AntennaIDs: []llrp.AntennaID{0}, // 0 = all antennas
			StopTrigger: llrp.AISpecStopTrigger{
				Trigger: llrp.AIStopTriggerNone, // bounded by ROSpec stop trigger
			},
			InventoryParameterSpecs: []llrp.InventoryParameterSpec{{
				InventoryParameterSpecID: 1,
				AirProtocolID:            llrp.AirProtoEPCGlobalClass1Gen2,
			}},
		}},
		ROReportSpec: &llrp.ROReportSpec{
			Trigger: llrp.NTagsOrROEnd,
			N:       0, // 0 + ROEnd trigger = "report on RO end"
			TagReportContentSelector: llrp.TagReportContentSelector{
				EnableAntennaID:    true,
				EnablePeakRSSI:     true,
				EnableTagSeenCount: true,
				// EPC96 / EPCData come along automatically — that's the
				// payload we actually use. The other Enable* flags
				// above are cheap context for future analytics.
			},
		},
	}
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
