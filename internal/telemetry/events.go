package telemetry

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// cryptoRandRead is a package var so span-id generation is deterministic under
// test (tests can swap it to feed fixed bytes). Production reads crypto/rand.
var cryptoRandRead = rand.Read

// TelemetryEvent is the queue payload schema consumed by ff-pipeline telemetry.
type TelemetryEvent struct {
	TraceID      string                 `json:"trace_id"`
	SpanID       string                 `json:"span_id"`
	ParentSpanID string                 `json:"parent_span_id,omitempty"`
	Name         string                 `json:"name"`
	Service      string                 `json:"service"`
	StartTimeMS  int64                  `json:"start_time_ms"`
	DurationMS   int64                  `json:"duration_ms"`
	Outcome      string                 `json:"outcome"`
	Error        string                 `json:"error,omitempty"`
	Attrs        map[string]interface{} `json:"attrs"`
}

// Emitter batches telemetry and posts to supervisor /internal/telemetry.
type Emitter struct {
	supervisorURL string
	token         string
	client        *http.Client

	mu    sync.Mutex
	batch []TelemetryEvent
}

// NewQueueEmitterFromEnv returns a best-effort emitter configured from env.
// GC_TELEMETRY_URL wins. Otherwise we derive from GC_BEAD_STORE_URL.
func NewQueueEmitterFromEnv() *Emitter {
	supervisorURL := strings.TrimSpace(os.Getenv("GC_TELEMETRY_URL"))
	if supervisorURL == "" {
		beadURL := strings.TrimSpace(os.Getenv("GC_BEAD_STORE_URL"))
		if i := strings.Index(beadURL, "/internal/bead-store/"); i > 0 {
			supervisorURL = beadURL[:i]
		}
	}
	return &Emitter{
		supervisorURL: strings.TrimRight(supervisorURL, "/"),
		token:         strings.TrimSpace(os.Getenv("GC_SUPERVISOR_TOKEN")),
		client:        &http.Client{Timeout: 3 * time.Second},
	}
}

func (e *Emitter) Enabled() bool {
	return e != nil && e.supervisorURL != "" && e.token != ""
}

// NewEmitterForTest builds an enabled Emitter pointed at supervisorURL with a
// fixed token. It exists so other packages can exercise molecule-lifecycle
// emission against a recording test server without exporting the struct fields.
func NewEmitterForTest(supervisorURL string, client *http.Client) *Emitter {
	if client == nil {
		client = &http.Client{Timeout: 3 * time.Second}
	}
	return &Emitter{
		supervisorURL: strings.TrimRight(supervisorURL, "/"),
		token:         "test-token",
		client:        client,
	}
}

func (e *Emitter) Emit(event TelemetryEvent) {
	if e == nil || !e.Enabled() {
		return
	}
	e.mu.Lock()
	e.batch = append(e.batch, event)
	e.mu.Unlock()
}

// Molecule lifecycle event names (WP-OBS-4). These form a second root span in
// Honeycomb, correlated to the ff-pipeline dispatch span via shared trace_id.
const (
	EventMoleculeStart    = "molecule.start"
	EventStepStart        = "step.start"
	EventStepComplete     = "step.complete"
	EventStepFail         = "step.fail"
	EventStepTimeout      = "step.timeout"
	EventFidelityRun      = "fidelity.run"
	EventFidelityVerdict  = "fidelity.verdict"
	EventMoleculeComplete = "molecule.complete"
)

// MoleculeEvent describes a single molecule-lifecycle telemetry emission. It is
// a thin, transport-agnostic carrier so the dispatch path can describe an event
// without hand-building a TelemetryEvent (and without knowing the trace-boundary
// rule). Build it, hand it to Emitter.EmitMolecule, and the emitter stamps the
// shared identity fields (trace_id, span_id, service) and the start/duration
// timing.
type MoleculeEvent struct {
	// Name is one of the Event* constants above.
	Name string
	// TraceID is the molecule's gc.trace_id (the only correlation link back to
	// the ff-pipeline dispatch span). Required.
	TraceID string
	// Root marks this event as the molecule's root span. When true, ParentSpanID
	// is never set regardless of any inherited span context (the dispatch span is
	// already closed; in Honeycomb the molecule trace links via trace_id
	// equality, NOT a parent pointer). molecule.start sets this true.
	Root bool
	// Outcome is the OTel span outcome ("", "ok", "error", "timeout").
	Outcome string
	// DurationMS is the span duration. Zero for instantaneous .start events.
	DurationMS int64
	// Error is an optional error string surfaced on the span.
	Error string
	// Attrs are the event-specific attributes. gc_trace_id is added automatically.
	Attrs map[string]interface{}
}

// EmitMolecule stamps the shared span identity onto a MoleculeEvent and queues
// it. trace_id is always copied into attrs as gc_trace_id so every molecule
// event self-describes its correlation key. The trace-boundary rule (no
// parent_span_id on a root span) is enforced here, not at the call site: a root
// event never carries a parent pointer because the dispatch span it correlates
// to is already closed.
func (e *Emitter) EmitMolecule(ev MoleculeEvent) {
	if e == nil || !e.Enabled() || ev.TraceID == "" {
		return
	}
	attrs := map[string]interface{}{}
	for k, v := range ev.Attrs {
		attrs[k] = v
	}
	attrs["gc_trace_id"] = ev.TraceID

	out := TelemetryEvent{
		TraceID:     ev.TraceID,
		SpanID:      NewSpanID(),
		Name:        ev.Name,
		Service:     "gascity",
		StartTimeMS: time.Now().UnixMilli() - ev.DurationMS,
		DurationMS:  ev.DurationMS,
		Outcome:     ev.Outcome,
		Error:       ev.Error,
		Attrs:       attrs,
	}
	// Trace-boundary rule: a molecule root span never points at the (closed)
	// dispatch span. Non-root molecule events are left without a parent too;
	// Honeycomb groups the molecule trace by trace_id equality, not by parent
	// edges into the dispatch trace. ParentSpanID stays empty in both cases.
	_ = ev.Root

	e.Emit(out)
}

// NewSpanID returns a random 8-byte hex span id for molecule lifecycle spans.
func NewSpanID() string {
	var buf [8]byte
	if _, err := cryptoRandRead(buf[:]); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(buf[:])
}

func (e *Emitter) Flush() error {
	if e == nil || !e.Enabled() {
		return nil
	}
	e.mu.Lock()
	events := append([]TelemetryEvent(nil), e.batch...)
	e.batch = nil
	e.mu.Unlock()
	if len(events) == 0 {
		return nil
	}

	body, err := json.Marshal(events)
	if err != nil {
		fmt.Fprintf(os.Stderr, "telemetry flush marshal: %v\n", err) //nolint:errcheck
		return err
	}
	req, err := http.NewRequest(http.MethodPost, e.supervisorURL+"/internal/telemetry", bytes.NewReader(body))
	if err != nil {
		fmt.Fprintf(os.Stderr, "telemetry flush request: %v\n", err) //nolint:errcheck
		return err
	}
	req.Header.Set("Authorization", "Bearer "+e.token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := e.client.Do(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "telemetry flush send: %v\n", err) //nolint:errcheck
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		err = fmt.Errorf("telemetry flush status=%d", resp.StatusCode)
		fmt.Fprintf(os.Stderr, "%v\n", err) //nolint:errcheck
		return err
	}
	return nil
}
