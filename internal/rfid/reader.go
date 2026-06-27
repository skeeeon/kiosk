// Package rfid wraps an LLRP RFID reader (e.g. an Impinj R700) behind
// a small domain interface the rest of the kiosk binary can mock for
// tests. The underlying LLRP protocol implementation comes from
// EdgeX's device-rfid-llrp-go/pkg/llrp; that import is an
// implementation detail of impinjReader and is not re-exported.
//
// One inventory cycle per ReadFor call: AddROSpec / EnableROSpec /
// collect ROAccessReport / DisableROSpec / DeleteROSpec. A supervisor
// goroutine maintains a healthy LLRP connection across reader reboots
// and network blips — see Connect. When the operator lists antennas in
// config, the kiosk also queries GET_READER_CAPABILITIES on each
// connect to resolve requested dBm against the reader's actual power
// table and embeds the per-antenna config inline in the ROSpec, so a
// reader power-cycle does not require re-pushing baseline state. See
// docs/rfid.md for the full design.
package rfid

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"math"
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
	// Connect starts a supervisor goroutine that maintains an LLRP
	// session to the reader, retrying on failure with exponential
	// backoff until Close is called. The error return is reserved for
	// programmer errors (e.g. calling Connect twice) — connectivity
	// failures are handled by the supervisor, not surfaced here. The
	// caller can assume the reader is "wiring up in the background" as
	// soon as Connect returns; ReadFor calls during a gap return
	// ErrNotConnected.
	Connect() error

	// ReadFor runs a single inventory cycle for the given duration and
	// returns the set of observed EPCs. Returns ErrNotConnected if the
	// supervisor has not yet established a session (or has lost it and
	// is retrying).
	ReadFor(ctx context.Context, d time.Duration) ([]EPC, error)

	// Close shuts the supervisor down cleanly and tears down any
	// in-flight LLRP session. Safe to call when Connect was never run
	// or already failed — in that case it's a no-op. Idempotent.
	Close() error

	// Connected reports whether the supervisor currently holds a live
	// LLRP session. It's a point-in-time read for operational metrics —
	// a true result can go stale the instant the connection drops, so
	// callers use it for display, not for gating ReadFor (which has its
	// own ErrNotConnected path).
	Connected() bool
}

// ErrNotConnected is returned by ReadFor when the supervisor has not
// yet established a session — either because Connect was never called,
// or because the reader is down and the supervisor is backing off
// between retries. Callers branch on this if they want different
// behavior from generic LLRP errors (e.g. a kiosk that boots without
// the reader plugged in but should still serve manual scans).
var ErrNotConnected = errors.New("rfid: not connected")

const (
	// dialTimeout caps how long a single connect attempt waits for
	// the TCP handshake. The supervisor retries on failure so this is
	// per-attempt, not total.
	dialTimeout = 5 * time.Second

	// shutdownTimeout caps each phase of Close (graceful Shutdown,
	// supervisor join, drain of the connection goroutine).
	shutdownTimeout = 2 * time.Second

	// capsTimeout caps the GET_READER_CAPABILITIES round-trip we send
	// on every connect when the operator has configured antennas. If
	// the reader is reachable but silent here, treat it as a connect
	// failure and let the supervisor retry — better than running with
	// stale/zero power indices.
	capsTimeout = 5 * time.Second

	// reconnectMinBackoff is the supervisor's initial retry delay
	// after a failed connect. Reset to this value after every
	// successful connect.
	reconnectMinBackoff = 1 * time.Second

	// reconnectMaxBackoff caps the supervisor's retry delay. Chosen to
	// keep "reader is back" recovery within half a minute on the worst
	// of timing while not hammering a misconfigured endpoint.
	reconnectMaxBackoff = 30 * time.Second
)

// New returns a Reader configured from one reader's config block. It does
// not dial yet — call Connect for that. The caller gates on rfid.enabled and
// constructs one Reader per entry in the rfid.readers map. Returns an error
// only for static config problems the config package's own validation should
// have already caught; in practice this is a defense-in-depth check.
func New(cfg config.RFIDReaderConfig) (Reader, error) {
	if cfg.Host == "" || cfg.Port == 0 {
		return nil, errors.New("rfid: reader host/port not configured")
	}
	antennas := make([]configuredAntenna, 0, len(cfg.Antennas))
	for _, a := range cfg.Antennas {
		if a.ID < 1 || a.ID > math.MaxUint16 {
			return nil, fmt.Errorf("rfid: antenna id %d out of range", a.ID)
		}
		antennas = append(antennas, configuredAntenna{
			id:         uint16(a.ID),
			txPowerDBm: a.TxPowerDBm,
		})
	}
	return &impinjReader{
		addr:     net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port)),
		antennas: antennas,
	}, nil
}

// configuredAntenna is the immutable cfg-derived per-port intent.
// Power is in dBm; the LLRP wire wants a 1-based index into a
// reader-specific power table, which we resolve on each connect.
type configuredAntenna struct {
	id         uint16
	txPowerDBm float64
}

// txConfig is the per-connection result of fetching GET_READER_CAPABILITIES
// and resolving the operator's dBm requests against the reader's actual
// power table. nil when the operator did not configure antennas — the
// ROSpec then falls back to "all antennas, reader's own baseline."
type txConfig struct {
	hopTableID   uint16
	channelIndex uint16
	antennas     []resolvedAntenna
}

type resolvedAntenna struct {
	id         uint16
	powerIndex uint16
}

// impinjReader is the production Reader. The naming reflects that we
// only test against Impinj R-series hardware in v1 — the wire protocol
// is LLRP-standard, but vendor quirks (Impinj custom messages, etc.)
// belong here when we add them. The underlying *llrp.Client is
// vendor-agnostic.
type impinjReader struct {
	addr     string
	antennas []configuredAntenna

	// supervisor lifecycle. supCancel/supDone are set in Connect and
	// cleared in Close. Both nil = not connected (or already closed).
	supCancel context.CancelFunc
	supDone   chan struct{}

	// mu guards the connection-tied fields below. The supervisor swaps
	// them as a set on each successful connect, and clears them on
	// disconnect.
	mu       sync.Mutex
	client   *llrp.Client
	conn     net.Conn
	connDone chan error // goroutine running client.Connect signals exit here
	txCfg    *txConfig

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

// Connect starts the supervisor goroutine. Returns immediately; the
// supervisor handles the actual dial + handshake + capabilities query
// in the background, retrying with exponential backoff on failure
// until Close is called. The supervisor owns its own lifetime (a
// context tied to Close), so Connect takes no context of its own.
func (r *impinjReader) Connect() error {
	r.mu.Lock()
	if r.supCancel != nil {
		r.mu.Unlock()
		return errors.New("rfid: already connected")
	}
	supCtx, cancel := context.WithCancel(context.Background())
	r.supCancel = cancel
	r.supDone = make(chan struct{})
	r.mu.Unlock()

	go r.supervisor(supCtx)
	return nil
}

// supervisor loops forever until ctx is cancelled (Close). Each
// iteration attempts a connect; on success it parks on the connection's
// done channel until either the connection drops (retry) or Close
// fires (exit). Exponential backoff applies to connect failures only —
// a healthy session that drops resets to the minimum.
func (r *impinjReader) supervisor(ctx context.Context) {
	defer close(r.supDone)

	backoff := reconnectMinBackoff

	for {
		if ctx.Err() != nil {
			return
		}

		if err := r.attemptConnect(ctx); err != nil {
			log.Printf("rfid: connect to %s failed: %v; retrying in %v", r.addr, err, backoff)
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return
			}
			backoff *= 2
			if backoff > reconnectMaxBackoff {
				backoff = reconnectMaxBackoff
			}
			continue
		}
		backoff = reconnectMinBackoff

		r.mu.Lock()
		done := r.connDone
		r.mu.Unlock()

		// done should never be nil here — attemptConnect publishes it
		// before returning success — but guard against a future
		// refactor that breaks that invariant rather than spin-looping.
		if done == nil {
			log.Printf("rfid: internal: no connDone after successful connect; exiting supervisor")
			return
		}

		select {
		case err := <-done:
			log.Printf("rfid: connection to %s dropped: %v; reconnecting", r.addr, err)
			r.clearConnection()
		case <-ctx.Done():
			// Close will tear down the live connection.
			return
		}
	}
}

// attemptConnect dials, starts the LLRP client goroutine, runs the
// capabilities round-trip (when antennas are configured), and on
// success publishes the new state under mu. On any failure it tears
// down whatever was half-built before returning so the supervisor's
// next iteration starts from a clean slate.
func (r *impinjReader) attemptConnect(ctx context.Context) error {
	dialCtx, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()

	conn, err := (&net.Dialer{}).DialContext(dialCtx, "tcp", r.addr)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}

	client := llrp.NewClient(
		llrp.WithMessageHandler(llrp.MsgROAccessReport,
			llrp.MessageHandlerFunc(r.onROAccessReport)),
	)
	done := make(chan error, 1)
	go func() {
		err := client.Connect(conn)
		if errors.Is(err, llrp.ErrClientClosed) {
			err = nil
		}
		done <- err
	}()

	txCfg, err := r.fetchAndResolveCaps(ctx, client)
	if err != nil {
		// Tear down the half-formed connection before returning. We
		// don't bother with Shutdown here — there's no graceful state
		// to preserve; the supervisor is going to retry.
		_ = client.Close()
		_ = conn.Close()
		select {
		case <-done:
		case <-time.After(shutdownTimeout):
		}
		return err
	}

	r.mu.Lock()
	r.client = client
	r.conn = conn
	r.connDone = done
	r.txCfg = txCfg
	r.mu.Unlock()

	if txCfg == nil {
		log.Printf("rfid: connected to %s (using reader's default antenna config)", r.addr)
	} else {
		log.Printf("rfid: connected to %s (%d antennas tuned)", r.addr, len(txCfg.antennas))
	}
	return nil
}

// fetchAndResolveCaps queries GET_READER_CAPABILITIES and translates
// the operator's per-antenna dBm requests into the LLRP wire-level
// power indices. Returns nil when no antennas are configured (the
// ROSpec then doesn't override anything and the reader uses its own
// baseline).
func (r *impinjReader) fetchAndResolveCaps(ctx context.Context, client *llrp.Client) (*txConfig, error) {
	if len(r.antennas) == 0 {
		return nil, nil
	}

	capsCtx, cancel := context.WithTimeout(ctx, capsTimeout)
	defer cancel()

	req := &llrp.GetReaderCapabilities{ReaderCapabilitiesRequestedData: llrp.ReaderCapAll}
	resp := &llrp.GetReaderCapabilitiesResponse{}
	if err := client.SendFor(capsCtx, req, resp); err != nil {
		return nil, fmt.Errorf("GET_READER_CAPABILITIES: %w", err)
	}
	if st := resp.Status(); st.Status != llrp.StatusSuccess {
		return nil, fmt.Errorf("GET_READER_CAPABILITIES status %d: %s", st.Status, st.ErrorDescription)
	}
	if resp.RegulatoryCapabilities == nil || resp.RegulatoryCapabilities.UHFBandCapabilities == nil {
		return nil, errors.New("reader returned no UHF band capabilities")
	}
	uhf := resp.RegulatoryCapabilities.UHFBandCapabilities
	if len(uhf.TransmitPowerLevels) == 0 {
		return nil, errors.New("reader returned empty transmit power table")
	}

	hopID, chanIdx := pickFrequency(uhf.FrequencyInformation)
	out := &txConfig{hopTableID: hopID, channelIndex: chanIdx}

	for _, a := range r.antennas {
		idx, dBm := nearestPowerIndex(uhf.TransmitPowerLevels, a.txPowerDBm)
		out.antennas = append(out.antennas, resolvedAntenna{id: a.id, powerIndex: idx})
		log.Printf("rfid: antenna %d requested %.1f dBm, using index %d (%.2f dBm)",
			a.id, a.txPowerDBm, idx, dBm)
	}
	return out, nil
}

// nearestPowerIndex picks the highest entry at or below want — never
// silently exceeding the requested ceiling. If every entry is above
// want (the request is below the reader's minimum), clamp to the
// reader's lowest available level and log; refusing to operate would
// be worse than running at the minimum the hardware supports.
//
// Returns the chosen index and its actual dBm value (for logging).
func nearestPowerIndex(table []llrp.TransmitPowerLevelTableEntry, want float64) (uint16, float64) {
	var bestIdx, lowIdx uint16
	bestDBm := math.Inf(-1)
	lowDBm := math.Inf(1)
	found := false
	for _, e := range table {
		dBm := float64(e.TransmitPowerValue) / 100.0
		if dBm <= want && (!found || dBm > bestDBm) {
			bestIdx = e.Index
			bestDBm = dBm
			found = true
		}
		if dBm < lowDBm {
			lowIdx = e.Index
			lowDBm = dBm
		}
	}
	if found {
		return bestIdx, bestDBm
	}
	log.Printf("rfid: requested %.1f dBm is below reader minimum %.2f dBm; clamping", want, lowDBm)
	return lowIdx, lowDBm
}

// pickFrequency derives the HopTableID + ChannelIndex pair that
// RFTransmitter expects. In hopping regions (e.g. FCC) the HopTableID
// is meaningful and ChannelIndex is unused; in fixed-frequency regions
// (e.g. ETSI) ChannelIndex is the 1-based offset into the fixed table
// and HopTableID is unused. When the reader returns neither, we pass
// zeros and let it apply its own defaults.
func pickFrequency(info llrp.FrequencyInformation) (hopTableID, channelIndex uint16) {
	if info.Hopping {
		if len(info.FrequencyHopTables) > 0 {
			return uint16(info.FrequencyHopTables[0].HopTableID), 0
		}
		return 0, 0
	}
	if info.FixedFrequencyTable != nil && len(info.FixedFrequencyTable.Frequencies) > 0 {
		return 0, 1
	}
	return 0, 0
}

// ReadFor runs one LLRP inventory cycle for d and returns the
// deduplicated EPCs observed. The flow:
//
//  1. Snapshot the supervisor-published client + txCfg; if no client,
//     return ErrNotConnected so callers fall back to barcode gracefully.
//  2. Reset the in-impinjReader accumulator to a fresh slice.
//  3. Send AddROSpec; check LLRPStatus.
//  4. Send EnableROSpec; check LLRPStatus. The ROSpec auto-starts
//     (ROStartTriggerImmediate) and auto-stops after d ms
//     (ROStopTriggerDuration). ROAccessReport messages flow inbound
//     during that window and the handler accumulates EPCs.
//  5. Wait for d, or for ctx to cancel — whichever first.
//  6. Send DisableROSpec + DeleteROSpec, best-effort, on a fresh
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
	txCfg := r.txCfg
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

	spec := buildROSpec(d, txCfg)

	// Add + Enable. Failures on either short-circuit; cleanup is
	// best-effort but we still try, because the reader may have
	// accepted Add even if Enable failed.
	if err := sendAndCheck(ctx, client, spec.Add(), &llrp.AddROSpecResponse{}); err != nil {
		r.cleanupSpec(client, spec) // no-op if Add never landed
		return nil, fmt.Errorf("rfid: AddROSpec: %w", err)
	}
	if err := sendAndCheck(ctx, client, spec.Enable(), &llrp.EnableROSpecResponse{}); err != nil {
		r.cleanupSpec(client, spec)
		return nil, fmt.Errorf("rfid: EnableROSpec: %w", err)
	}

	// Wait for the read window or ctx cancel. The ROSpec stops itself
	// on the reader side after d ms, but we still send Disable below
	// so a torn read leaves clean state for next time.
	select {
	case <-time.After(d):
	case <-ctx.Done():
	}

	r.cleanupSpec(client, spec)

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
// so it runs even when the caller's ctx is dead. Errors are intentional
// drops — the caller already knows the read window completed; whether
// the reader's state is perfectly clean is best-effort.
func (r *impinjReader) cleanupSpec(client *llrp.Client, spec *llrp.ROSpec) {
	cleanupCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = sendAndCheck(cleanupCtx, client, spec.Disable(), &llrp.DisableROSpecResponse{})
	_ = sendAndCheck(cleanupCtx, client, spec.Delete(), &llrp.DeleteROSpecResponse{})
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
// counter_scan / enclosure_diff cycle.
const rfidROSpecID = 1

// buildROSpec returns a minimal ROSpec configured to start
// immediately, run for d, deliver results when the RO ends, and
// inventory the configured antennas using EPC Gen2. When txCfg is nil
// (operator didn't configure antennas) the ROSpec uses AntennaIDs={0}
// — "all antennas, reader's own baseline" — and emits no per-antenna
// override. When non-nil, the ROSpec carries one AntennaConfiguration
// per listed antenna with its resolved TransmitPowerIndex, so a reader
// reboot does not require re-pushing baseline state: every ReadFor
// re-applies the tuning.
func buildROSpec(d time.Duration, txCfg *txConfig) *llrp.ROSpec {
	ms := uint32(d / time.Millisecond)
	if ms == 0 {
		ms = 1 // 0 would mean "never stop"; clamp defensively
	}

	inv := llrp.InventoryParameterSpec{
		InventoryParameterSpecID: 1,
		AirProtocolID:            llrp.AirProtoEPCGlobalClass1Gen2,
	}

	var antennaIDs []llrp.AntennaID
	if txCfg == nil || len(txCfg.antennas) == 0 {
		antennaIDs = []llrp.AntennaID{0} // 0 = all antennas
	} else {
		for _, a := range txCfg.antennas {
			antennaIDs = append(antennaIDs, llrp.AntennaID(a.id))
			inv.AntennaConfigurations = append(inv.AntennaConfigurations,
				llrp.AntennaConfiguration{
					AntennaID: llrp.AntennaID(a.id),
					RFTransmitter: &llrp.RFTransmitter{
						HopTableID:         txCfg.hopTableID,
						ChannelIndex:       txCfg.channelIndex,
						TransmitPowerIndex: a.powerIndex,
					},
				})
		}
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
			AntennaIDs: antennaIDs,
			StopTrigger: llrp.AISpecStopTrigger{
				Trigger: llrp.AIStopTriggerNone, // bounded by ROSpec stop trigger
			},
			InventoryParameterSpecs: []llrp.InventoryParameterSpec{inv},
		}},
		ROReportSpec: &llrp.ROReportSpec{
			Trigger: llrp.NTagsOrROEnd,
			N:       0, // 0 + ROEnd trigger = "report on RO end"
			TagReportContentSelector: llrp.TagReportContentSelector{
				EnableAntennaID:    true,
				EnablePeakRSSI:     true,
				EnableTagSeenCount: true,
			},
		},
	}
}

// Close cancels the supervisor, waits for it to exit, then tears down
// any live LLRP session with a graceful Shutdown round-trip followed
// by a hard Close. Safe to call when Connect was never run, and
// idempotent — a second call is a no-op.
func (r *impinjReader) Close() error {
	r.mu.Lock()
	cancel := r.supCancel
	supDone := r.supDone
	r.supCancel = nil
	r.supDone = nil
	r.mu.Unlock()

	if cancel == nil {
		return nil
	}
	cancel()
	if supDone != nil {
		select {
		case <-supDone:
		case <-time.After(shutdownTimeout):
		}
	}

	// Supervisor has exited (or timed out). Tear down whichever
	// connection it had live at exit time, if any.
	r.mu.Lock()
	client := r.client
	conn := r.conn
	done := r.connDone
	r.client = nil
	r.conn = nil
	r.connDone = nil
	r.txCfg = nil
	r.mu.Unlock()

	if client == nil {
		return nil
	}

	shutdownCtx, sc := context.WithTimeout(context.Background(), shutdownTimeout)
	defer sc()
	_ = client.Shutdown(shutdownCtx)
	_ = client.Close()
	if conn != nil {
		_ = conn.Close()
	}
	if done != nil {
		select {
		case <-done:
		case <-time.After(shutdownTimeout):
		}
	}
	return nil
}

// Connected reports whether a live LLRP session is currently held. Reads the
// same mutex-guarded conn the supervisor swaps on connect/disconnect, so it's
// an honest point-in-time signal for metrics.
func (r *impinjReader) Connected() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.conn != nil
}

// clearConnection nils the connection-tied state without trying to
// gracefully shut down — used by the supervisor when the read goroutine
// has already exited because the underlying connection dropped. The
// client object is already dead; there's nothing to shut down
// gracefully.
func (r *impinjReader) clearConnection() {
	r.mu.Lock()
	r.client = nil
	r.conn = nil
	r.connDone = nil
	r.txCfg = nil
	r.mu.Unlock()
}
