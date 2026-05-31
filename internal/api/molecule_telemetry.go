package api

import (
	"strconv"
	"strings"
	"sync"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/telemetry"
)

// WP-OBS-4: molecule.start emission.
//
// molecule.start is raised in the api layer because the workflow root bead is
// created here (execSling), which is the only place the molecule's gc.trace_id
// is known at creation time. The remaining molecule lifecycle events (step.*,
// fidelity.*, molecule.complete) are emitted from the dispatch path in cmd/gc.
// Both layers build their emitter the same way — once, from the environment —
// so a single gc process shares one telemetry transport configuration.
var (
	apiMoleculeEmitterOnce sync.Once
	apiMoleculeEmitter     *telemetry.Emitter
)

func apiMoleculeObsEmitter() *telemetry.Emitter {
	apiMoleculeEmitterOnce.Do(func() {
		if apiMoleculeEmitter == nil {
			apiMoleculeEmitter = telemetry.NewQueueEmitterFromEnv()
		}
	})
	return apiMoleculeEmitter
}

// setAPIMoleculeEmitter installs an emitter for tests. Returns a restore func.
func setAPIMoleculeEmitter(e *telemetry.Emitter) func() {
	apiMoleculeEmitterOnce.Do(func() {})
	prev := apiMoleculeEmitter
	apiMoleculeEmitter = e
	return func() { apiMoleculeEmitter = prev }
}

// emitMoleculeStartEvent emits the molecule.start root span for a freshly
// created workflow root bead.
//
// Trace-boundary rule (WP-OBS-4): molecule.start opens a SECOND root span. Its
// parent_span_id is deliberately NOT set — the ff-pipeline dispatch span that
// triggered this molecule is already closed, so there is no live parent to point
// at. The molecule trace links back to the dispatch trace ONLY via the shared
// trace_id (Honeycomb groups by trace_id equality, not by parent edges). Passing
// MoleculeEvent.Root=true makes the emitter enforce the empty parent pointer.
func emitMoleculeStartEvent(store beads.Store, rootBeadID, traceID string) {
	e := apiMoleculeObsEmitter()
	if e == nil || !e.Enabled() || strings.TrimSpace(traceID) == "" || rootBeadID == "" {
		return
	}
	root, err := store.Get(rootBeadID)
	if err != nil {
		return
	}
	factoryAttempt := 1
	if v, perr := strconv.Atoi(strings.TrimSpace(root.Metadata["gc.factory_attempt"])); perr == nil && v > 0 {
		factoryAttempt = v
	}
	e.EmitMolecule(telemetry.MoleculeEvent{
		Name:    telemetry.EventMoleculeStart,
		TraceID: strings.TrimSpace(traceID),
		Root:    true,
		Outcome: "ok",
		Attrs: map[string]interface{}{
			"form_id":         strings.TrimSpace(root.Metadata["gc.form_id"]),
			"fn_id":           strings.TrimSpace(root.Metadata["gc.fn_id"]),
			"factory_attempt": factoryAttempt,
		},
	})
}
