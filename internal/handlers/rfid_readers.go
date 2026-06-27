package handlers

import (
	"sort"

	"github.com/skeeeon/kiosk/internal/config"
	"github.com/skeeeon/kiosk/internal/rfid"
)

// ReaderHandle bundles a constructed LLRP reader with the per-reader config
// metadata the handlers need at read time (its mode, and for enclosure_diff
// the cabinet it covers). One handle per entry in cfg.RFID.Readers, built in
// cmd/kiosk/main.go. Reader is nil when the startup New() failed — the handle
// still exists so callers can resolve the reader's identity and surface a 503.
type ReaderHandle struct {
	Reader      rfid.Reader // nil if New failed at startup
	ID          string      // the reader_id key from cfg.RFID.Readers (the gateway id for sightings)
	Mode        string      // counter_scan | enclosure_diff
	EnclosureID string      // the cabinet this reader covers (enclosure_diff)
	Zone        string      // optional coarse location label; a read here stamps last-observed (L1)
}

// ReaderByID returns the configured reader handle for id. When id is empty and
// exactly one reader is configured, that sole reader is returned — the
// single-reader kiosk case, where selection is implicit. With multiple readers
// an empty id is ambiguous (ok=false); explicit per-terminal selection arrives
// with the terminal work. Unknown id → ok=false.
func (h *Handlers) ReaderByID(id string) (*ReaderHandle, bool) {
	if len(h.Readers) == 0 {
		return nil, false
	}
	if id == "" {
		if len(h.Readers) == 1 {
			for _, hd := range h.Readers {
				return hd, true
			}
		}
		return nil, false
	}
	hd, ok := h.Readers[id]
	return hd, ok
}

// ReaderForEnclosure returns the enclosure_diff reader covering enclosureID.
// It prefers an exact enclosure_id match (the counter-plus-cabinets node) and
// falls back to the sole reader (the single-cabinet kiosk, where the cart's
// enclosure_id and the lone reader trivially line up).
func (h *Handlers) ReaderForEnclosure(enclosureID string) (*ReaderHandle, bool) {
	if enclosureID != "" {
		for _, hd := range h.Readers {
			if hd.Mode == config.RFIDModeEnclosureDiff && hd.EnclosureID == enclosureID {
				return hd, true
			}
		}
	}
	return h.ReaderByID("")
}

// enclosureCount returns the number of distinct enclosures (by enclosure_id)
// this node's readers cover. More than one means enclosure_diff reads must be
// partitioned by enclosure_id (each cabinet diffs only its own inventory);
// zero or one means the whole serialized inventory belongs to the sole cabinet,
// so no partition filter is applied — single-cabinet kiosks and not-yet-assigned
// instances keep working unchanged.
func (h *Handlers) enclosureCount() int {
	seen := make(map[string]struct{}, len(h.Readers))
	for _, hd := range h.Readers {
		if hd.Mode == config.RFIDModeEnclosureDiff && hd.EnclosureID != "" {
			seen[hd.EnclosureID] = struct{}{}
		}
	}
	return len(seen)
}

// anyReaderConnected reports whether at least one configured reader currently
// holds a live LLRP session. Point-in-time, for the operational metrics gauge.
func (h *Handlers) anyReaderConnected() bool {
	for _, hd := range h.Readers {
		if hd.Reader != nil && hd.Reader.Connected() {
			return true
		}
	}
	return false
}

// ReaderConfigSnapshot is one reader's config + live connection status, for the
// read-only config.snapshot command. Pure observability — no secrets, no
// mutation.
type ReaderConfigSnapshot struct {
	ReaderID    string `json:"reader_id"`
	Mode        string `json:"mode"`
	Host        string `json:"host"`
	Port        int    `json:"port"`
	EnclosureID string `json:"enclosure_id,omitempty"`
	Antennas    int    `json:"antennas"`
	Connected   bool   `json:"connected"`
}

// RFIDConfigSnapshot is the node's RFID topology as the controller's Readers tab
// renders it: the shared read_window plus one row per configured reader (sorted
// by reader_id for a stable display), each tagged with its current LLRP
// connection status.
type RFIDConfigSnapshot struct {
	Enabled      bool                   `json:"enabled"`
	ReadWindowMs int64                  `json:"read_window_ms"`
	Readers      []ReaderConfigSnapshot `json:"readers"`
}

// RFIDConfigSnapshot builds the snapshot from config (host/port/mode/
// enclosure_id/antennas) joined with the live reader handles (connected).
func (h *Handlers) RFIDConfigSnapshot() RFIDConfigSnapshot {
	out := RFIDConfigSnapshot{
		Enabled:      h.Cfg.RFID.Enabled,
		ReadWindowMs: h.Cfg.RFID.ReadWindow.AsDuration().Milliseconds(),
		Readers:      []ReaderConfigSnapshot{},
	}
	for id, rc := range h.Cfg.RFID.Readers {
		connected := false
		if hd := h.Readers[id]; hd != nil && hd.Reader != nil {
			connected = hd.Reader.Connected()
		}
		out.Readers = append(out.Readers, ReaderConfigSnapshot{
			ReaderID:    id,
			Mode:        rc.Mode,
			Host:        rc.Host,
			Port:        rc.Port,
			EnclosureID: rc.EnclosureID,
			Antennas:    len(rc.Antennas),
			Connected:   connected,
		})
	}
	sort.Slice(out.Readers, func(i, j int) bool { return out.Readers[i].ReaderID < out.Readers[j].ReaderID })
	return out
}
