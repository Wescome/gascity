package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/molecule"
	"github.com/gastownhall/gascity/internal/telemetry"
)

// recordingTelemetryServer captures every telemetry batch POSTed to it so tests
// can assert on the molecule lifecycle events the dispatch path emits.
type recordingTelemetryServer struct {
	*httptest.Server
	mu     sync.Mutex
	events []telemetry.TelemetryEvent
}

func newRecordingTelemetryServer(t *testing.T) *recordingTelemetryServer {
	t.Helper()
	rec := &recordingTelemetryServer{}
	rec.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var batch []telemetry.TelemetryEvent
		if err := json.NewDecoder(r.Body).Decode(&batch); err != nil {
			t.Errorf("decode telemetry: %v", err)
		}
		rec.mu.Lock()
		rec.events = append(rec.events, batch...)
		rec.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(rec.Close)
	return rec
}

func (rec *recordingTelemetryServer) byName(name string) []telemetry.TelemetryEvent {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	var out []telemetry.TelemetryEvent
	for _, e := range rec.events {
		if e.Name == name {
			out = append(out, e)
		}
	}
	return out
}

// installTestMoleculeEmitter wires the cmd/gc molecule emitter to a recording
// server and flushes leftover events on cleanup.
func installTestMoleculeEmitter(t *testing.T, rec *recordingTelemetryServer) {
	t.Helper()
	e := telemetry.NewEmitterForTest(rec.URL, rec.Client())
	restore := setMoleculeEmitter(e)
	t.Cleanup(func() {
		_ = e.Flush()
		restore()
	})
}

// A passing non-release step emits step.start then step.complete, both carrying
// the molecule trace_id resolved from the root bead and the provider id.
func TestDispatchEmitsStepStartAndComplete(t *testing.T) {
	rec := newRecordingTelemetryServer(t)
	installTestMoleculeEmitter(t, rec)

	store := beads.NewMemStore()
	cfg, _, cleanup := seedHarnessRegistry(t, "pi-rpc", []string{"ai_reasoning", "model_routing"}, 1)
	defer cleanup()

	// Root bead carries the trace_id (stamped by the api layer at sling time).
	root, err := store.Create(beads.Bead{
		Title: "molecule root",
		Type:  "task",
		Metadata: map[string]string{
			"gc.trace_id": "trace-xyz",
			"gc.form_id":  "FORM-7",
			"gc.fn_id":    "FN-7",
		},
	})
	if err != nil {
		t.Fatalf("Create root: %v", err)
	}
	bead, err := store.Create(beads.Bead{
		Title: "Implement change",
		Type:  "task",
		Metadata: map[string]string{
			molecule.RuntimeRequirementsMetadataKey: "ai_reasoning,model_routing",
			"gc.step_ref":                           "implement",
			"gc.root_bead_id":                       root.ID,
		},
	})
	if err != nil {
		t.Fatalf("Create step: %v", err)
	}

	if _, err := maybeDispatchHarness(context.Background(), store, bead, cfg, t.TempDir(), io.Discard); err != nil {
		t.Fatalf("maybeDispatchHarness: %v", err)
	}
	// Force delivery (production flushes at molecule.complete; this step is not a
	// release step so we flush manually to inspect the batch).
	if err := moleculeObsEmitter().Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}

	starts := rec.byName(telemetry.EventStepStart)
	if len(starts) != 1 {
		t.Fatalf("step.start count = %d, want 1", len(starts))
	}
	if starts[0].TraceID != "trace-xyz" {
		t.Errorf("step.start trace_id = %q, want trace-xyz", starts[0].TraceID)
	}
	if starts[0].Attrs["step"] != "implement" {
		t.Errorf("step.start step attr = %v, want implement", starts[0].Attrs["step"])
	}
	if starts[0].Attrs["provider"] != "pi-rpc" {
		t.Errorf("step.start provider attr = %v, want pi-rpc", starts[0].Attrs["provider"])
	}
	if starts[0].Attrs["bead_id"] != bead.ID {
		t.Errorf("step.start bead_id attr = %v, want %s", starts[0].Attrs["bead_id"], bead.ID)
	}

	completes := rec.byName(telemetry.EventStepComplete)
	if len(completes) != 1 {
		t.Fatalf("step.complete count = %d, want 1", len(completes))
	}
	if completes[0].Outcome != "ok" {
		t.Errorf("step.complete outcome = %q, want ok", completes[0].Outcome)
	}
	if completes[0].Attrs["gc_trace_id"] != "trace-xyz" {
		t.Errorf("step.complete gc_trace_id = %v, want trace-xyz", completes[0].Attrs["gc_trace_id"])
	}
}

// When the molecule trace_id cannot be resolved (no root, no direct trace_id),
// no molecule events are emitted — dispatch still proceeds.
func TestDispatchWithoutTraceIDEmitsNothing(t *testing.T) {
	rec := newRecordingTelemetryServer(t)
	installTestMoleculeEmitter(t, rec)

	store := beads.NewMemStore()
	cfg, _, cleanup := seedHarnessRegistry(t, "pi-rpc", []string{"ai_reasoning"}, 1)
	defer cleanup()

	bead, err := store.Create(beads.Bead{
		Title: "Orphan step",
		Type:  "task",
		Metadata: map[string]string{
			molecule.RuntimeRequirementsMetadataKey: "ai_reasoning",
			"gc.step_ref":                           "implement",
			// no gc.root_bead_id and no gc.trace_id
		},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if _, err := maybeDispatchHarness(context.Background(), store, bead, cfg, t.TempDir(), io.Discard); err != nil {
		t.Fatalf("maybeDispatchHarness: %v", err)
	}
	_ = moleculeObsEmitter().Flush()

	if got := len(rec.byName(telemetry.EventStepStart)); got != 0 {
		t.Errorf("expected no step.start without a resolvable trace_id, got %d", got)
	}
}

// moleculeTraceID resolves directly from a bead's own gc.trace_id, and via the
// root bead when only gc.root_bead_id is present.
func TestMoleculeTraceIDResolution(t *testing.T) {
	store := beads.NewMemStore()
	root, err := store.Create(beads.Bead{
		Title:    "root",
		Type:     "task",
		Metadata: map[string]string{"gc.trace_id": "tid-root"},
	})
	if err != nil {
		t.Fatalf("Create root: %v", err)
	}

	direct := beads.Bead{ID: "b1", Metadata: map[string]string{"gc.trace_id": "tid-direct"}}
	if got := moleculeTraceID(store, direct); got != "tid-direct" {
		t.Errorf("direct trace_id = %q, want tid-direct", got)
	}

	child := beads.Bead{ID: "b2", Metadata: map[string]string{"gc.root_bead_id": root.ID}}
	if got := moleculeTraceID(store, child); got != "tid-root" {
		t.Errorf("child trace_id via root = %q, want tid-root", got)
	}

	orphan := beads.Bead{ID: "b3", Metadata: map[string]string{}}
	if got := moleculeTraceID(store, orphan); got != "" {
		t.Errorf("orphan trace_id = %q, want empty", got)
	}
}
