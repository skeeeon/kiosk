package demoseed

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"

	"github.com/skeeeon/kiosk/internal/events"
)

// commandTimeout mirrors the controller's own per-command budget. Local
// NATS RTT is sub-millisecond and a DB write on the kiosk side returns in
// tens of ms; five seconds is the same headroom the SPA's remote-admin
// path allows.
const commandTimeout = 5 * time.Second

// catalogWaitTimeout bounds how long ApplyRemote waits for the catalogue
// to reach a kiosk before giving up. The path is controller save → KV
// publish → kiosk watcher → local upsert, which is fast but asynchronous;
// a kiosk that hasn't started yet never arrives at all, and that is the
// failure worth naming clearly rather than hanging on.
const catalogWaitTimeout = 60 * time.Second

// catalogPollInterval is how often the wait re-asks.
const catalogPollInterval = 2 * time.Second

// commandEnvelope is the {success, error, data} reply every kiosk command
// handler writes. Same shape the controller's HTTP endpoints decode.
type commandEnvelope struct {
	Success bool            `json:"success"`
	Error   string          `json:"error,omitempty"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// ErrKioskOffline is what a timeout or a missing responder becomes. The
// usual cause during a demo stand-up is running the remote applier before
// the kiosks are started, which is the one ordering the runbook insists on.
var ErrKioskOffline = errors.New("kiosk_offline")

// ApplyRemote installs every kiosk node's local state over the command
// bus, from the controller, with no HTTP request and no heartbeat registry.
//
// The kiosk's dispatcher lands each command on the very function
// ApplyLocal calls directly — PerformStockAdjustment,
// PerformSetReorderThreshold, PerformCreate — so the two appliers cannot
// drift. That is the whole design: the remote applier is the local applier
// with NATS in the middle.
//
// Every unit provisioned this way lands with source=controller in the
// kiosk's instance_audit and an instance.lifecycle event on the bus. That
// is not a demo artefact — the controller really did provision them.
//
// Idempotency is read-then-write, matching ApplyLocal: the two snapshot
// commands say what is already there, and only the gaps are sent. The
// command_id column can't help here — a second run generates fresh ids,
// since its job is deduping a redelivery within one attempt, not two
// attempts a week apart.
//
// It sends three commands, one per kind of kiosk-local state:
// inventory.adjust for quantities, inventory.set_threshold for low-stock
// levels, and instance.create for serialized units. All three land on the
// same functions ApplyLocal calls in-process, so the estate is identical
// whichever applier built it.
func ApplyRemote(nc *nats.Conn, adminID string, log Logf) (*Result, error) {
	if log == nil {
		log = discard
	}
	if adminID == "" {
		return nil, errors.New("controller admin id is required: it is stamped on every audit row the kiosks write")
	}

	out := &Result{}
	for _, node := range Nodes {
		if node.Timeclock {
			continue
		}
		inventory, err := waitForCatalog(nc, node, catalogWaitTimeout, log)
		if err != nil {
			return nil, err
		}
		if err := applyStockRemote(nc, node, adminID, inventory, out, log); err != nil {
			return nil, err
		}
		if err := applyUnitsRemote(nc, node, adminID, out, log); err != nil {
			return nil, err
		}
	}

	// Drain before the caller closes the connection. A command-sending
	// seeder that returns immediately loses whatever is still in flight;
	// seed-catalog's `defer pub.Close()` is safe only because it never
	// sends a request.
	if err := nc.Flush(); err != nil {
		return nil, fmt.Errorf("flush: %w", err)
	}
	return out, nil
}

// waitForCatalog blocks until a kiosk's local items table holds every SKU
// this node is supposed to stock, then returns that kiosk's current
// on-hand quantities so the caller can skip the SKUs already at target.
//
// This is the ordering hazard worth understanding rather than memorising:
// `instance.create` resolves item_code against the kiosk's OWN items
// table, and the catalogue reaches it asynchronously (controller save → KV
// → watcher → local upsert). Firing the commands straight after the
// catalogue write is a race the seeder would lose most of the time.
//
// On failure the error names the kiosk and the SKUs that never arrived,
// because "timed out" on its own sends an operator to the wrong place.
func waitForCatalog(nc *nats.Conn, node Node, timeout time.Duration, log Logf) (map[string]stockState, error) {
	want := node.ItemCodes()
	if len(want) == 0 {
		return map[string]stockState{}, nil
	}

	deadline := time.Now().Add(timeout)
	var missing []string
	for {
		onHand, err := inventorySnapshot(nc, node.Code)
		switch {
		case err == nil:
			missing = missing[:0]
			for _, c := range want {
				if _, ok := onHand[c]; !ok {
					missing = append(missing, c)
				}
			}
			if len(missing) == 0 {
				log("demo-seed: %s has all %d SKUs", node.Code, len(want))
				return onHand, nil
			}
		case errors.Is(err, ErrKioskOffline):
			missing = append(missing[:0], want...)
		default:
			return nil, fmt.Errorf("%s: inventory.snapshot: %w", node.Code, err)
		}

		if time.Now().After(deadline) {
			sort.Strings(missing)
			return nil, fmt.Errorf("%s never received %d of %d SKUs after %s (missing: %s) — is the kiosk running and pointed at this broker?",
				node.Code, len(missing), len(want), timeout, strings.Join(missing, ", "))
		}
		log("demo-seed: waiting for %s (%d of %d SKUs still missing)", node.Code, len(missing), len(want))
		time.Sleep(catalogPollInterval)
	}
}

// stockState is what a kiosk currently holds for one SKU. Both fields are
// read before writing so a re-run sends nothing it does not need to.
type stockState struct {
	Quantity  int
	Threshold int
}

// inventorySnapshot returns the kiosk's item_code → current state map.
func inventorySnapshot(nc *nats.Conn, kioskCode string) (map[string]stockState, error) {
	data, err := request(nc, events.InventorySnapshotCommandSubject(kioskCode), struct{}{})
	if err != nil {
		return nil, err
	}
	var reply struct {
		Items []struct {
			ItemCode         string `json:"item_code"`
			QuantityOnHand   int    `json:"quantity_on_hand"`
			ReorderThreshold int    `json:"reorder_threshold"`
		} `json:"items"`
	}
	if err := json.Unmarshal(data, &reply); err != nil {
		return nil, fmt.Errorf("decode inventory snapshot: %w", err)
	}
	out := make(map[string]stockState, len(reply.Items))
	for _, it := range reply.Items {
		out[it.ItemCode] = stockState{Quantity: it.QuantityOnHand, Threshold: it.ReorderThreshold}
	}
	return out, nil
}

// instanceCodes returns the instance codes already present at a kiosk.
// Asking is cheaper and far more honest than firing instance.create and
// pattern-matching the unique-constraint text out of the error string.
func instanceCodes(nc *nats.Conn, kioskCode string) (map[string]bool, error) {
	data, err := request(nc, events.InstanceSnapshotCommandSubject(kioskCode), struct{}{})
	if err != nil {
		return nil, err
	}
	var reply struct {
		Instances []struct {
			InstanceCode string `json:"instance_code"`
		} `json:"instances"`
	}
	if err := json.Unmarshal(data, &reply); err != nil {
		return nil, fmt.Errorf("decode instance snapshot: %w", err)
	}
	out := make(map[string]bool, len(reply.Instances))
	for _, in := range reply.Instances {
		out[in.InstanceCode] = true
	}
	return out, nil
}

func applyStockRemote(nc *nats.Conn, node Node, adminID string, current map[string]stockState, out *Result, log Logf) error {
	for _, s := range node.Stock {
		have, known := current[s.ItemCode]

		if known && have.Quantity == s.Quantity {
			out.Existing++
		} else {
			payload := map[string]any{
				"command_id":          uuid.NewString(),
				"controller_admin_id": adminID,
				"item_code":           s.ItemCode,
				// Absolute, not delta: the fixture states what the shelf
				// holds, not how much to add to whatever the demo traffic
				// has already done to it.
				"mode":   "absolute",
				"value":  s.Quantity,
				"reason": SeedReason,
			}
			if _, err := request(nc, events.InventoryAdjustCommandSubject(node.Code), payload); err != nil {
				return fmt.Errorf("%s inventory.adjust %s: %w", node.Code, s.ItemCode, err)
			}
			out.Applied++
			log("demo-seed: %s %s qty -> %d", node.Code, s.ItemCode, s.Quantity)
		}

		// The threshold is its own command because it is kiosk-local and
		// does not ride the catalogue wire. Sending it here is what lets a
		// managed estate raise a low-stock alert at all — and it is why the
		// runbook no longer needs a per-kiosk `kiosk demo-seed` pass.
		if known && have.Threshold == s.ReorderThreshold {
			out.Existing++
			continue
		}
		payload := map[string]any{
			"controller_admin_id": adminID,
			"item_code":           s.ItemCode,
			"value":               s.ReorderThreshold,
		}
		if _, err := request(nc, events.InventorySetThresholdCommandSubject(node.Code), payload); err != nil {
			return fmt.Errorf("%s inventory.set_threshold %s: %w", node.Code, s.ItemCode, err)
		}
		out.Applied++
		log("demo-seed: %s %s reorder_threshold -> %d", node.Code, s.ItemCode, s.ReorderThreshold)
	}
	return nil
}

func applyUnitsRemote(nc *nats.Conn, node Node, adminID string, out *Result, log Logf) error {
	if len(node.Units) == 0 {
		return nil
	}
	present, err := instanceCodes(nc, node.Code)
	if err != nil {
		return fmt.Errorf("%s instance.snapshot: %w", node.Code, err)
	}
	for _, u := range node.Units {
		if present[u.Code] {
			out.Existing++
			continue
		}
		payload := map[string]any{
			"command_id":          uuid.NewString(),
			"controller_admin_id": adminID,
			"item_code":           u.ItemCode,
			"code":                u.Code,
			"serial":              u.Serial,
			"rfid_epc":            u.EPC(),
			"enclosure_id":        u.EnclosureID,
			"notes":               SeedReason,
		}
		if _, err := request(nc, events.InstanceCreateCommandSubject(node.Code), payload); err != nil {
			return fmt.Errorf("%s instance.create %s: %w", node.Code, u.Code, err)
		}
		out.Applied++
		log("demo-seed: %s unit %s (%s) in cabinet %s", node.Code, u.Code, u.ItemCode, orNone(u.EnclosureID))
	}
	return nil
}

// request sends one command and unwraps the envelope. A command message
// with no reply inbox is dropped by the kiosk's dispatcher without ever
// reaching a handler, so everything here goes out as request/reply — never
// a bare publish.
func request(nc *nats.Conn, subject string, payload any) (json.RawMessage, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	msg, err := nc.Request(subject, body, commandTimeout)
	if err != nil {
		if errors.Is(err, nats.ErrTimeout) || errors.Is(err, nats.ErrNoResponders) {
			return nil, ErrKioskOffline
		}
		return nil, err
	}
	var env commandEnvelope
	if err := json.Unmarshal(msg.Data, &env); err != nil {
		return nil, fmt.Errorf("decode reply: %w", err)
	}
	if !env.Success {
		if env.Error == "" {
			return nil, errors.New("kiosk returned an unsuccessful reply with no error text")
		}
		return nil, errors.New(env.Error)
	}
	return env.Data, nil
}
