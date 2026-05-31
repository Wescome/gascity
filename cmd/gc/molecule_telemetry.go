package main

import (
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/telemetry"
)

// WP-OBS-4: molecule lifecycle telemetry.
//
// The dispatch path (maybeDispatchHarness, runFidelityValidator) runs as
// standalone functions invoked by the control-dispatch command, not from a live
// CityRuntime, so there is no Emitter threaded through their signatures. We
// mirror the package-function telemetry convention used elsewhere in this binary
// (telemetry.RecordX(context.Background(), ...)): a process-wide molecule emitter
// built once from the environment. It is best-effort and a no-op when the
// telemetry transport is not configured (Enabled()==false), exactly like the
// startup emitter.
var (
	moleculeEmitterOnce sync.Once
	moleculeEmitter     *telemetry.Emitter
)

// moleculeObsEmitter returns the process-wide molecule lifecycle emitter,
// constructed once from env. It is overridable in tests via setMoleculeEmitter.
func moleculeObsEmitter() *telemetry.Emitter {
	moleculeEmitterOnce.Do(func() {
		if moleculeEmitter == nil {
			moleculeEmitter = telemetry.NewQueueEmitterFromEnv()
		}
	})
	return moleculeEmitter
}

// setMoleculeEmitter installs an emitter for tests and marks the once as done so
// the env-derived emitter is not built. Returns a restore func.
func setMoleculeEmitter(e *telemetry.Emitter) func() {
	moleculeEmitterOnce.Do(func() {}) // consume the once so production init is skipped
	prev := moleculeEmitter
	moleculeEmitter = e
	return func() { moleculeEmitter = prev }
}

// moleculeTraceID resolves the molecule's gc.trace_id. The step bead carries
// gc.root_bead_id; the root bead carries gc.trace_id (stamped at sling time,
// WP-OBS-3). A step bead that is itself the root carries gc.trace_id directly.
// Returns "" when the trace id cannot be resolved — callers drop the event.
func moleculeTraceID(store beads.Store, bead beads.Bead) string {
	if tid := strings.TrimSpace(bead.Metadata["gc.trace_id"]); tid != "" {
		return tid
	}
	rootID := strings.TrimSpace(bead.Metadata["gc.root_bead_id"])
	if rootID == "" || rootID == bead.ID || store == nil {
		return ""
	}
	root, err := store.Get(rootID)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(root.Metadata["gc.trace_id"])
}

// moleculeFactoryAttempt reads gc.factory_attempt as an int attr, defaulting to
// the contract default of 1.
func moleculeFactoryAttempt(bead beads.Bead) int {
	if v, err := strconv.Atoi(strings.TrimSpace(bead.Metadata["gc.factory_attempt"])); err == nil && v > 0 {
		return v
	}
	return defaultFidelityFactoryAttempt
}

// Note: molecule.start is emitted from the api layer (execSling), where the
// workflow root bead is created and its gc.trace_id is first known. See
// internal/api/molecule_telemetry.go. The trace-boundary rule (no parent_span_id
// on the molecule root span) is documented there and enforced by the emitter.

// emitStepStart fires when a step bead is dispatched to a provider.
func emitStepStart(store beads.Store, bead beads.Bead, provider string) {
	e := moleculeObsEmitter()
	if e == nil || !e.Enabled() {
		return
	}
	traceID := moleculeTraceID(store, bead)
	if traceID == "" {
		return
	}
	e.EmitMolecule(telemetry.MoleculeEvent{
		Name:    telemetry.EventStepStart,
		TraceID: traceID,
		Attrs: map[string]interface{}{
			"step":     harnessStepName(bead),
			"bead_id":  bead.ID,
			"provider": provider,
		},
	})
}

// emitStepComplete fires when a step bead closes pass.
func emitStepComplete(store beads.Store, bead beads.Bead, provider string, started time.Time) {
	e := moleculeObsEmitter()
	if e == nil || !e.Enabled() {
		return
	}
	traceID := moleculeTraceID(store, bead)
	if traceID == "" {
		return
	}
	e.EmitMolecule(telemetry.MoleculeEvent{
		Name:       telemetry.EventStepComplete,
		TraceID:    traceID,
		Outcome:    "ok",
		DurationMS: time.Since(started).Milliseconds(),
		Attrs: map[string]interface{}{
			"step":     harnessStepName(bead),
			"bead_id":  bead.ID,
			"provider": provider,
		},
	})
}

// emitStepFail fires when a step bead closes fail (non-timeout).
func emitStepFail(store beads.Store, bead beads.Bead, failureReason string) {
	e := moleculeObsEmitter()
	if e == nil || !e.Enabled() {
		return
	}
	traceID := moleculeTraceID(store, bead)
	if traceID == "" {
		return
	}
	e.EmitMolecule(telemetry.MoleculeEvent{
		Name:    telemetry.EventStepFail,
		TraceID: traceID,
		Outcome: "error",
		Error:   failureReason,
		Attrs: map[string]interface{}{
			"step":           harnessStepName(bead),
			"bead_id":        bead.ID,
			"failure_reason": failureReason,
		},
	})
}

// emitStepTimeout fires when a step exceeds its deadline (distinct from fail).
func emitStepTimeout(store beads.Store, bead beads.Bead, started time.Time) {
	e := moleculeObsEmitter()
	if e == nil || !e.Enabled() {
		return
	}
	traceID := moleculeTraceID(store, bead)
	if traceID == "" {
		return
	}
	elapsed := time.Since(started).Milliseconds()
	e.EmitMolecule(telemetry.MoleculeEvent{
		Name:       telemetry.EventStepTimeout,
		TraceID:    traceID,
		Outcome:    "timeout",
		DurationMS: elapsed,
		Attrs: map[string]interface{}{
			"step":       harnessStepName(bead),
			"bead_id":    bead.ID,
			"elapsed_ms": elapsed,
		},
	})
}

// emitFidelityRun fires when the release step invokes the fidelity validator.
func emitFidelityRun(store beads.Store, bead beads.Bead, priorStepCount int) {
	e := moleculeObsEmitter()
	if e == nil || !e.Enabled() {
		return
	}
	traceID := moleculeTraceID(store, bead)
	if traceID == "" {
		return
	}
	e.EmitMolecule(telemetry.MoleculeEvent{
		Name:    telemetry.EventFidelityRun,
		TraceID: traceID,
		Attrs: map[string]interface{}{
			"bead_id":          bead.ID,
			"prior_step_count": priorStepCount,
		},
	})
}

// emitFidelityVerdict fires when the fidelity validator exits.
func emitFidelityVerdict(store beads.Store, bead beads.Bead, verdict string, started time.Time) {
	e := moleculeObsEmitter()
	if e == nil || !e.Enabled() {
		return
	}
	traceID := moleculeTraceID(store, bead)
	if traceID == "" {
		return
	}
	outcome := "ok"
	if verdict == "fail_closed" {
		outcome = "error"
	}
	e.EmitMolecule(telemetry.MoleculeEvent{
		Name:       telemetry.EventFidelityVerdict,
		TraceID:    traceID,
		Outcome:    outcome,
		DurationMS: time.Since(started).Milliseconds(),
		Attrs: map[string]interface{}{
			"bead_id": bead.ID,
			"verdict": verdict,
		},
	})
}

// emitMoleculeComplete fires when the molecule reaches a terminal outcome at the
// release step. It is the batch flush point for the molecule: after emitting it,
// the emitter is flushed so the whole molecule's events leave the process in one
// POST (WP-OBS-4 flush rule).
func emitMoleculeComplete(store beads.Store, bead beads.Bead, outcome string, started time.Time) {
	e := moleculeObsEmitter()
	if e == nil || !e.Enabled() {
		return
	}
	traceID := moleculeTraceID(store, bead)
	if traceID == "" {
		return
	}
	spanOutcome := "ok"
	if outcome != "approved" && outcome != "released" {
		spanOutcome = "error"
	}
	e.EmitMolecule(telemetry.MoleculeEvent{
		Name:       telemetry.EventMoleculeComplete,
		TraceID:    traceID,
		Outcome:    spanOutcome,
		DurationMS: time.Since(started).Milliseconds(),
		Attrs: map[string]interface{}{
			"form_id":         strings.TrimSpace(bead.Metadata["gc.form_id"]),
			"outcome":         outcome,
			"factory_attempt": moleculeFactoryAttempt(bead),
		},
	})
	// Flush rule (WP-OBS-4): molecule.complete is the per-molecule batch flush
	// point. Best-effort — a transport failure is already logged inside Flush.
	_ = e.Flush()
}
