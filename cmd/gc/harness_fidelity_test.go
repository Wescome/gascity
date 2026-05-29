package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/harness"
)

// writeFakeFidelityScript writes a bash script that exits with the given code
// and points fidelityReleaseScriptPath at it for the duration of the test. The
// script echoes a marker so stdout/stderr piping is observable.
func writeFakeFidelityScript(t *testing.T, exitCode int) {
	t.Helper()
	dir := t.TempDir()
	script := filepath.Join(dir, "fidelity-release.sh")
	body := "#!/usr/bin/env bash\necho fidelity-script-ran\nexit " + strconv.Itoa(exitCode) + "\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatalf("write fake script: %v", err)
	}
	prev := fidelityReleaseScriptPath
	fidelityReleaseScriptPath = script
	t.Cleanup(func() { fidelityReleaseScriptPath = prev })
}

// fidelityTestBead returns a bead with full lineage metadata and a rig root
// pointed at a fresh temp directory so the job file lands somewhere writable.
func fidelityTestBead(t *testing.T) beads.Bead {
	t.Helper()
	rigRoot := t.TempDir()
	return beads.Bead{
		ID: "bead-fidelity-1",
		Metadata: map[string]string{
			"gc.fn_id":                 "FN-GC-1",
			"gc.is_id":                 "IS-GC-1",
			"gc.es_id":                 "ES-GC-1",
			"gc.ep_id":                 "EP-GC-1",
			"gc.form_id":               "FORM-GC-1",
			"gc.factory_attempt":       "3",
			"gc.rig_root":              rigRoot,
			"gc.ff_webhook_url":        "https://factory.example/webhook",
			"gc.ff_webhook_hmac_keyid": "v2",
			"gc.max_iterations":        "7",
		},
	}
}

func readFidelityJob(t *testing.T, rigRoot string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(rigRoot, "fidelity-job.json"))
	if err != nil {
		t.Fatalf("read fidelity-job.json: %v", err)
	}
	var job map[string]any
	if err := json.Unmarshal(raw, &job); err != nil {
		t.Fatalf("unmarshal fidelity-job.json: %v", err)
	}
	return job
}

// The fidelity job is written with the contracted shape: lineage from bead
// metadata, the provider response embedded, and webhook wiring populated.
func TestFidelityValidatorWritesJobShape(t *testing.T) {
	writeFakeFidelityScript(t, 0)
	store := beads.NewMemStore()
	bead := fidelityTestBead(t)
	rigRoot := bead.Metadata["gc.rig_root"]
	resp := harness.ExecutionResponse{
		Status:           harness.StatusCompleted,
		ArtifactManifest: []harness.ManifestEntry{{Artifact: "out.txt", Status: harness.ManifestProduced}},
	}

	if err := runFidelityValidator(context.Background(), store, bead, &config.City{}, t.TempDir(), resp, &bytes.Buffer{}); err != nil {
		t.Fatalf("runFidelityValidator: %v", err)
	}

	job := readFidelityJob(t, rigRoot)
	if job["step_name"] != "release" {
		t.Errorf("step_name = %v, want release", job["step_name"])
	}
	if job["is_release_step"] != true {
		t.Errorf("is_release_step = %v, want true", job["is_release_step"])
	}

	lineage, ok := job["lineage"].(map[string]any)
	if !ok {
		t.Fatalf("lineage missing or wrong type: %T", job["lineage"])
	}
	if lineage["fn_id"] != "FN-GC-1" {
		t.Errorf("lineage.fn_id = %v, want FN-GC-1", lineage["fn_id"])
	}
	if lineage["is_id"] != "IS-GC-1" {
		t.Errorf("lineage.is_id = %v, want IS-GC-1", lineage["is_id"])
	}
	if lineage["es_id"] != "ES-GC-1" {
		t.Errorf("lineage.es_id = %v, want ES-GC-1", lineage["es_id"])
	}
	if lineage["ep_id"] != "EP-GC-1" {
		t.Errorf("lineage.ep_id = %v, want EP-GC-1", lineage["ep_id"])
	}
	if lineage["form_id"] != "FORM-GC-1" {
		t.Errorf("lineage.form_id = %v, want FORM-GC-1", lineage["form_id"])
	}
	if lineage["bead_id"] != "bead-fidelity-1" {
		t.Errorf("lineage.bead_id = %v, want bead-fidelity-1", lineage["bead_id"])
	}

	respMap, ok := job["response"].(map[string]any)
	if !ok {
		t.Fatalf("response missing or wrong type: %T", job["response"])
	}
	if respMap["status"] != "completed" {
		t.Errorf("response.status = %v, want completed", respMap["status"])
	}

	conv, ok := job["convergence"].(map[string]any)
	if !ok {
		t.Fatalf("convergence missing or wrong type: %T", job["convergence"])
	}
	if conv["max_amendment_depth"].(float64) != 7 {
		t.Errorf("convergence.max_amendment_depth = %v, want 7", conv["max_amendment_depth"])
	}

	webhook, ok := job["webhook"].(map[string]any)
	if !ok {
		t.Fatalf("webhook missing or wrong type: %T", job["webhook"])
	}
	if webhook["url"] != "https://factory.example/webhook" {
		t.Errorf("webhook.url = %v, want https://factory.example/webhook", webhook["url"])
	}
	if webhook["key_id"] != "v2" {
		t.Errorf("webhook.key_id = %v, want v2", webhook["key_id"])
	}
}

// factory_attempt is a bare JSON integer, never a quoted string.
func TestFidelityValidatorFactoryAttemptIsInteger(t *testing.T) {
	writeFakeFidelityScript(t, 0)
	store := beads.NewMemStore()
	bead := fidelityTestBead(t)
	rigRoot := bead.Metadata["gc.rig_root"]

	if err := runFidelityValidator(context.Background(), store, bead, &config.City{}, t.TempDir(), harness.ExecutionResponse{Status: harness.StatusCompleted}, &bytes.Buffer{}); err != nil {
		t.Fatalf("runFidelityValidator: %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(rigRoot, "fidelity-job.json"))
	if err != nil {
		t.Fatalf("read job: %v", err)
	}
	// A bare integer cannot be wrapped in quotes in the serialized JSON.
	if bytes.Contains(raw, []byte(`"factory_attempt": "3"`)) || bytes.Contains(raw, []byte(`"factory_attempt":"3"`)) {
		t.Fatalf("factory_attempt serialized as a string, want bare integer; json=%s", raw)
	}

	job := readFidelityJob(t, rigRoot)
	lineage := job["lineage"].(map[string]any)
	attempt, ok := lineage["factory_attempt"].(float64)
	if !ok {
		t.Fatalf("factory_attempt type = %T, want number", lineage["factory_attempt"])
	}
	if attempt != 3 {
		t.Errorf("factory_attempt = %v, want 3", attempt)
	}
}

// policy_events must serialize as [] not null when the response carries none.
func TestFidelityValidatorPolicyEventsEmptyArray(t *testing.T) {
	writeFakeFidelityScript(t, 0)
	store := beads.NewMemStore()
	bead := fidelityTestBead(t)
	rigRoot := bead.Metadata["gc.rig_root"]

	if err := runFidelityValidator(context.Background(), store, bead, &config.City{}, t.TempDir(), harness.ExecutionResponse{Status: harness.StatusCompleted}, &bytes.Buffer{}); err != nil {
		t.Fatalf("runFidelityValidator: %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(rigRoot, "fidelity-job.json"))
	if err != nil {
		t.Fatalf("read job: %v", err)
	}
	if bytes.Contains(raw, []byte(`"policy_events": null`)) || bytes.Contains(raw, []byte(`"policy_events":null`)) {
		t.Fatalf("policy_events serialized as null, want []; json=%s", raw)
	}

	job := readFidelityJob(t, rigRoot)
	respMap := job["response"].(map[string]any)
	events, ok := respMap["policy_events"].([]any)
	if !ok {
		t.Fatalf("policy_events type = %T, want array", respMap["policy_events"])
	}
	if len(events) != 0 {
		t.Errorf("policy_events len = %d, want 0", len(events))
	}
}

// Script exit 0 = RELEASE posted and accepted; the bead is closed by the
// driver, so runFidelityValidator returns nil.
func TestFidelityValidatorExitZeroReturnsNil(t *testing.T) {
	writeFakeFidelityScript(t, 0)
	store := beads.NewMemStore()
	bead := fidelityTestBead(t)

	if err := runFidelityValidator(context.Background(), store, bead, &config.City{}, t.TempDir(), harness.ExecutionResponse{Status: harness.StatusCompleted}, &bytes.Buffer{}); err != nil {
		t.Fatalf("expected nil on exit 0, got %v", err)
	}
}

// Script exit 10 = revise; the molecule re-enters at the Code step. The
// verdict is stamped and ErrFidelityRevise is returned.
func TestFidelityValidatorExitTenReturnsRevise(t *testing.T) {
	writeFakeFidelityScript(t, 10)
	store := beads.NewMemStore()
	created, err := store.Create(beads.Bead{Title: "fidelity revise", Type: "task"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	bead := fidelityTestBead(t)
	bead.ID = created.ID

	err = runFidelityValidator(context.Background(), store, bead, &config.City{}, t.TempDir(), harness.ExecutionResponse{Status: harness.StatusCompleted}, &bytes.Buffer{})
	if !errors.Is(err, ErrFidelityRevise) {
		t.Fatalf("expected ErrFidelityRevise, got %v", err)
	}
	updated, err := store.Get(created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if updated.Metadata["gc.harness_fidelity_verdict"] != "revise" {
		t.Errorf("gc.harness_fidelity_verdict = %q, want revise", updated.Metadata["gc.harness_fidelity_verdict"])
	}
}

// Script exit 20 = fail-closed; molecule.failed has been POSTed. The verdict
// is stamped and ErrFidelityFailClosed is returned.
func TestFidelityValidatorExitTwentyReturnsFailClosed(t *testing.T) {
	writeFakeFidelityScript(t, 20)
	store := beads.NewMemStore()
	created, err := store.Create(beads.Bead{Title: "fidelity fail", Type: "task"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	bead := fidelityTestBead(t)
	bead.ID = created.ID

	err = runFidelityValidator(context.Background(), store, bead, &config.City{}, t.TempDir(), harness.ExecutionResponse{Status: harness.StatusFailed}, &bytes.Buffer{})
	if !errors.Is(err, ErrFidelityFailClosed) {
		t.Fatalf("expected ErrFidelityFailClosed, got %v", err)
	}
	updated, err := store.Get(created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if updated.Metadata["gc.harness_fidelity_verdict"] != "fail_closed" {
		t.Errorf("gc.harness_fidelity_verdict = %q, want fail_closed", updated.Metadata["gc.harness_fidelity_verdict"])
	}
	if updated.Status != "closed" {
		t.Errorf("status = %q, want closed", updated.Status)
	}
	if updated.Metadata["gc.failure_reason"] != "fidelity_fail_closed" {
		t.Errorf("gc.failure_reason = %q, want fidelity_fail_closed", updated.Metadata["gc.failure_reason"])
	}
}

// Any other non-zero exit is treated as fail-closed.
func TestFidelityValidatorUnknownExitTreatedAsFailClosed(t *testing.T) {
	writeFakeFidelityScript(t, 3)
	store := beads.NewMemStore()
	created, err := store.Create(beads.Bead{Title: "fidelity weird exit", Type: "task"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	bead := fidelityTestBead(t)
	bead.ID = created.ID

	err = runFidelityValidator(context.Background(), store, bead, &config.City{}, t.TempDir(), harness.ExecutionResponse{Status: harness.StatusFailed}, &bytes.Buffer{})
	if !errors.Is(err, ErrFidelityFailClosed) {
		t.Fatalf("expected ErrFidelityFailClosed for unknown exit, got %v", err)
	}
	updated, err := store.Get(created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if updated.Status != "closed" {
		t.Errorf("status = %q, want closed", updated.Status)
	}
}

// A missing webhook URL must not panic: the job still serializes with an empty
// url and the script runs.
func TestFidelityValidatorMissingWebhookURLNoPanic(t *testing.T) {
	writeFakeFidelityScript(t, 0)
	store := beads.NewMemStore()
	rigRoot := t.TempDir()
	bead := beads.Bead{
		ID: "bead-no-webhook",
		Metadata: map[string]string{
			"gc.fn_id":    "FN-GC-2",
			"gc.rig_root": rigRoot,
			// no gc.ff_webhook_url, no gc.ff_webhook_hmac_keyid, no gc.factory_attempt
		},
	}

	if err := runFidelityValidator(context.Background(), store, bead, &config.City{}, t.TempDir(), harness.ExecutionResponse{Status: harness.StatusCompleted}, &bytes.Buffer{}); err != nil {
		t.Fatalf("runFidelityValidator with missing webhook URL: %v", err)
	}

	job := readFidelityJob(t, rigRoot)
	webhook := job["webhook"].(map[string]any)
	if webhook["url"] != "" {
		t.Errorf("webhook.url = %v, want empty string", webhook["url"])
	}
	// Default key_id is v1 when none is set.
	if webhook["key_id"] != "v1" {
		t.Errorf("webhook.key_id = %v, want v1 (default)", webhook["key_id"])
	}
	// Default factory_attempt is 1 when none is set / unparsable.
	lineage := job["lineage"].(map[string]any)
	if lineage["factory_attempt"].(float64) != 1 {
		t.Errorf("factory_attempt default = %v, want 1", lineage["factory_attempt"])
	}
}

// The rig root falls back to cityPath/rigs/<formula_id> when gc.rig_root is
// absent.
func TestFidelityValidatorRigRootFallback(t *testing.T) {
	writeFakeFidelityScript(t, 0)
	store := beads.NewMemStore()
	cityPath := t.TempDir()
	bead := beads.Bead{
		ID: "bead-fallback",
		Metadata: map[string]string{
			"gc.fn_id":      "FN-GC-3",
			"gc.formula_id": "FORMULA-X",
			// no gc.rig_root
		},
	}

	if err := runFidelityValidator(context.Background(), store, bead, &config.City{}, cityPath, harness.ExecutionResponse{Status: harness.StatusCompleted}, &bytes.Buffer{}); err != nil {
		t.Fatalf("runFidelityValidator: %v", err)
	}

	fallback := filepath.Join(cityPath, "rigs", "FORMULA-X")
	if _, err := os.Stat(filepath.Join(fallback, "fidelity-job.json")); err != nil {
		t.Fatalf("expected job at fallback rig root %s: %v", fallback, err)
	}
}
