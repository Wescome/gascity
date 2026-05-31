package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/telemetry"
)

func TestEmitMoleculeStartEvent(t *testing.T) {
	var mu sync.Mutex
	var events []telemetry.TelemetryEvent
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var batch []telemetry.TelemetryEvent
		if err := json.NewDecoder(r.Body).Decode(&batch); err != nil {
			t.Errorf("decode: %v", err)
		}
		mu.Lock()
		events = append(events, batch...)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	e := telemetry.NewEmitterForTest(srv.URL, srv.Client())
	restore := setAPIMoleculeEmitter(e)
	defer restore()

	store := beads.NewMemStore()
	root, err := store.Create(beads.Bead{
		Title: "molecule root",
		Type:  "task",
		Metadata: map[string]string{
			"gc.form_id":         "FORM-9",
			"gc.fn_id":           "FN-9",
			"gc.factory_attempt": "2",
		},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	emitMoleculeStartEvent(store, root.ID, "trace-start")
	if err := e.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(events) != 1 {
		t.Fatalf("want 1 molecule.start event, got %d", len(events))
	}
	got := events[0]
	if got.Name != telemetry.EventMoleculeStart {
		t.Errorf("name = %q, want %q", got.Name, telemetry.EventMoleculeStart)
	}
	if got.TraceID != "trace-start" {
		t.Errorf("trace_id = %q, want trace-start", got.TraceID)
	}
	// Trace-boundary rule: molecule.start is a root span with no parent pointer.
	if got.ParentSpanID != "" {
		t.Errorf("molecule.start parent_span_id = %q, want empty", got.ParentSpanID)
	}
	if got.Attrs["form_id"] != "FORM-9" {
		t.Errorf("form_id = %v, want FORM-9", got.Attrs["form_id"])
	}
	if got.Attrs["fn_id"] != "FN-9" {
		t.Errorf("fn_id = %v, want FN-9", got.Attrs["fn_id"])
	}
	// JSON round-trips numbers as float64.
	if fa, ok := got.Attrs["factory_attempt"].(float64); !ok || fa != 2 {
		t.Errorf("factory_attempt = %v (%T), want 2", got.Attrs["factory_attempt"], got.Attrs["factory_attempt"])
	}
}
