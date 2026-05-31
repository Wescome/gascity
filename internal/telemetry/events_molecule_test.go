package telemetry

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// newTestEmitter returns an Emitter wired to a recording test server so Flush
// delivers somewhere and Enabled() is true.
func newTestEmitter(t *testing.T) (*Emitter, *recordedBatches) {
	t.Helper()
	rec := &recordedBatches{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var events []TelemetryEvent
		if err := json.NewDecoder(r.Body).Decode(&events); err != nil {
			t.Errorf("decode telemetry batch: %v", err)
		}
		rec.add(events)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	e := &Emitter{
		supervisorURL: srv.URL,
		token:         "test-token",
		client:        srv.Client(),
	}
	if !e.Enabled() {
		t.Fatal("test emitter should be enabled")
	}
	return e, rec
}

type recordedBatches struct {
	mu      sync.Mutex
	batches [][]TelemetryEvent
}

func (r *recordedBatches) add(events []TelemetryEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.batches = append(r.batches, events)
}

func (r *recordedBatches) all() []TelemetryEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []TelemetryEvent
	for _, b := range r.batches {
		out = append(out, b...)
	}
	return out
}

func TestEmitMoleculeStampsTraceAndSpan(t *testing.T) {
	e, rec := newTestEmitter(t)

	e.EmitMolecule(MoleculeEvent{
		Name:    EventMoleculeStart,
		TraceID: "trace-abc",
		Root:    true,
		Attrs:   map[string]interface{}{"form_id": "FORM-1", "fn_id": "FN-1", "factory_attempt": 1},
	})
	if err := e.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}

	events := rec.all()
	if len(events) != 1 {
		t.Fatalf("want 1 event, got %d", len(events))
	}
	got := events[0]
	if got.Name != EventMoleculeStart {
		t.Errorf("name = %q, want %q", got.Name, EventMoleculeStart)
	}
	if got.TraceID != "trace-abc" {
		t.Errorf("trace_id = %q, want trace-abc", got.TraceID)
	}
	if got.Service != "gascity" {
		t.Errorf("service = %q, want gascity", got.Service)
	}
	if got.SpanID == "" {
		t.Error("span_id should be populated")
	}
	if got.Attrs["gc_trace_id"] != "trace-abc" {
		t.Errorf("attrs.gc_trace_id = %v, want trace-abc", got.Attrs["gc_trace_id"])
	}
}

// Trace-boundary rule: molecule.start is a root span and MUST NOT carry a
// parent_span_id pointing at the (already-closed) dispatch span.
func TestEmitMoleculeRootHasNoParentSpan(t *testing.T) {
	e, rec := newTestEmitter(t)

	e.EmitMolecule(MoleculeEvent{
		Name:    EventMoleculeStart,
		TraceID: "trace-root",
		Root:    true,
	})
	if err := e.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}

	events := rec.all()
	if len(events) != 1 {
		t.Fatalf("want 1 event, got %d", len(events))
	}
	if events[0].ParentSpanID != "" {
		t.Errorf("molecule.start must have empty parent_span_id, got %q", events[0].ParentSpanID)
	}
}

func TestEmitMoleculeRequiresTraceID(t *testing.T) {
	e, rec := newTestEmitter(t)

	// Missing trace_id is dropped: a molecule event with no correlation key is
	// useless and must never be emitted.
	e.EmitMolecule(MoleculeEvent{Name: EventStepStart, TraceID: ""})
	if err := e.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	if got := len(rec.all()); got != 0 {
		t.Errorf("event with empty trace_id should be dropped, got %d events", got)
	}
}

func TestEmitMoleculeDurationOutcome(t *testing.T) {
	e, rec := newTestEmitter(t)

	e.EmitMolecule(MoleculeEvent{
		Name:       EventStepComplete,
		TraceID:    "trace-dur",
		Outcome:    "ok",
		DurationMS: 1500,
		Attrs:      map[string]interface{}{"step": "code", "bead_id": "bd-1", "provider": "pi"},
	})
	if err := e.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}

	events := rec.all()
	if len(events) != 1 {
		t.Fatalf("want 1 event, got %d", len(events))
	}
	got := events[0]
	if got.DurationMS != 1500 {
		t.Errorf("duration_ms = %d, want 1500", got.DurationMS)
	}
	if got.Outcome != "ok" {
		t.Errorf("outcome = %q, want ok", got.Outcome)
	}
	if got.Attrs["step"] != "code" || got.Attrs["provider"] != "pi" {
		t.Errorf("attrs not preserved: %+v", got.Attrs)
	}
}

func TestEmitMoleculeDisabledIsNoop(t *testing.T) {
	var e *Emitter // nil emitter, Enabled() == false
	e.EmitMolecule(MoleculeEvent{Name: EventMoleculeStart, TraceID: "x"})
	// no panic == pass
}
