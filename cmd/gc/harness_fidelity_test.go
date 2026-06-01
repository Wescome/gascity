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
		Status: harness.StatusCompleted,
		Artifacts: []harness.Artifact{
			{Path: "out.txt", Size: 4, Checksum: "sha256:abcd"},
		},
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
	manifest, ok := respMap["artifact_manifest"].([]any)
	if !ok || len(manifest) != 1 {
		t.Fatalf("response.artifact_manifest = %#v, want one entry", respMap["artifact_manifest"])
	}
	entry, ok := manifest[0].(map[string]any)
	if !ok {
		t.Fatalf("response.artifact_manifest[0] wrong type: %T", manifest[0])
	}
	if entry["name"] != "out.txt" {
		t.Errorf("manifest name = %v, want out.txt", entry["name"])
	}
	if entry["state"] != "produced" {
		t.Errorf("manifest state = %v, want produced", entry["state"])
	}
	if entry["checksum"] != "sha256:abcd" {
		t.Errorf("manifest checksum = %v, want sha256:abcd", entry["checksum"])
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

func TestFidelityValidatorLineageFallsBackToDescription(t *testing.T) {
	bead := beads.Bead{
		ID:          "gc-release-1",
		Description: "**Factory lineage:** fn=FN-DESC is=IS-DESC es=ES-DESC ep=EP-DESC form=FORM-DESC attempt=3\n",
		Metadata: map[string]string{
			"gc.rig_root": t.TempDir(),
		},
	}

	job := buildFidelityJob(beads.NewMemStore(), bead, harness.ExecutionResponse{Status: harness.StatusCompleted})
	if job.Lineage.FnID != "FN-DESC" {
		t.Errorf("FnID = %q, want FN-DESC", job.Lineage.FnID)
	}
	if job.Lineage.IsID != "IS-DESC" {
		t.Errorf("IsID = %q, want IS-DESC", job.Lineage.IsID)
	}
	if job.Lineage.EsID != "ES-DESC" {
		t.Errorf("EsID = %q, want ES-DESC", job.Lineage.EsID)
	}
	if job.Lineage.EpID != "EP-DESC" {
		t.Errorf("EpID = %q, want EP-DESC", job.Lineage.EpID)
	}
	if job.Lineage.FormID != "FORM-DESC" {
		t.Errorf("FormID = %q, want FORM-DESC", job.Lineage.FormID)
	}
	if job.Lineage.FactoryAttempt != 3 {
		t.Errorf("FactoryAttempt = %d, want 3", job.Lineage.FactoryAttempt)
	}
	if job.Webhook.URL == "" {
		t.Fatalf("Webhook.URL is empty")
	}
}

// PriorStepVerdicts accumulates the serialized responses stamped on sibling
// beads sharing a gc.root_bead_id, excluding the Release step itself.
func TestFidelityPriorStepVerdictsAccumulatesSiblings(t *testing.T) {
	store := beads.NewMemStore()
	const root = "root-mol-1"

	// Two prior steps (Code, Verify) each carry a stamped response. The Release
	// bead carries a response too but must be excluded from its own envelope.
	// The verdict envelope must be PriorStepVerdict-shaped: each sibling becomes
	// {step_index, step_name, outcome, remediation} — gc.outcome=="pass" maps to
	// outcome="approved", step_name comes from gc.step_ref.
	codeResp, _ := json.Marshal(harness.ExecutionResponse{Status: harness.StatusCompleted, SessionArchiveRef: "code-archive"})
	verifyResp, _ := json.Marshal(harness.ExecutionResponse{Status: harness.StatusCompleted, SessionArchiveRef: "verify-archive"})
	releaseResp, _ := json.Marshal(harness.ExecutionResponse{Status: harness.StatusCompleted, SessionArchiveRef: "release-archive"})

	code, _ := store.Create(beads.Bead{Title: "code step", Type: "task"})
	_ = store.Update(code.ID, beads.UpdateOpts{Metadata: map[string]string{
		"gc.root_bead_id":          root,
		"gc.step_ref":              "code",
		"gc.outcome":               "pass",
		"gc.harness_response_json": string(codeResp),
	}})
	verify, _ := store.Create(beads.Bead{Title: "verify step", Type: "task"})
	_ = store.Update(verify.ID, beads.UpdateOpts{Metadata: map[string]string{
		"gc.root_bead_id":          root,
		"gc.step_ref":              "verify",
		"gc.outcome":               "pass",
		"gc.harness_response_json": string(verifyResp),
	}})
	release, _ := store.Create(beads.Bead{Title: "release step", Type: "task"})
	_ = store.Update(release.ID, beads.UpdateOpts{Metadata: map[string]string{
		"gc.root_bead_id":          root,
		"gc.step_ref":              "release",
		"gc.harness_response_json": string(releaseResp),
	}})

	releaseBead, _ := store.Get(release.ID)
	verdicts := fidelityPriorStepVerdicts(store, releaseBead, nil)
	if len(verdicts) != 2 {
		t.Fatalf("prior_step_verdicts len = %d, want 2 (Code+Verify, Release excluded)", len(verdicts))
	}

	// Verdicts are earliest-first: index 0 = code, index 1 = verify.
	wantNames := []string{"code", "verify"}
	for i, v := range verdicts {
		m, ok := v.(map[string]any)
		if !ok {
			t.Fatalf("verdict[%d] wrong type: %T", i, v)
		}
		if got := m["step_index"]; got != i {
			t.Errorf("verdict[%d] step_index = %v, want %d", i, got, i)
		}
		if got := m["step_name"]; got != wantNames[i] {
			t.Errorf("verdict[%d] step_name = %v, want %q", i, got, wantNames[i])
		}
		if got := m["outcome"]; got != "approved" {
			t.Errorf("verdict[%d] outcome = %v, want approved", i, got)
		}
		if got := m["remediation"]; got != "" {
			t.Errorf("verdict[%d] remediation = %v, want empty", i, got)
		}
		if _, hasRaw := m["session_archive_ref"]; hasRaw {
			t.Errorf("verdict[%d] leaked raw ExecutionResponse field session_archive_ref", i)
		}
	}
}

// A sibling whose gc.outcome is not "pass" maps to outcome="revise".
func TestFidelityPriorStepVerdictsNonPassMapsToRevise(t *testing.T) {
	store := beads.NewMemStore()
	const root = "root-mol-revise"

	resp, _ := json.Marshal(harness.ExecutionResponse{Status: harness.StatusCompleted})
	code, _ := store.Create(beads.Bead{Title: "code step", Type: "task"})
	_ = store.Update(code.ID, beads.UpdateOpts{Metadata: map[string]string{
		"gc.root_bead_id":          root,
		"gc.step_ref":              "code",
		"gc.outcome":               "fail",
		"gc.harness_response_json": string(resp),
	}})
	release, _ := store.Create(beads.Bead{Title: "release step", Type: "task"})
	_ = store.Update(release.ID, beads.UpdateOpts{Metadata: map[string]string{
		"gc.root_bead_id":          root,
		"gc.step_ref":              "release",
		"gc.harness_response_json": string(resp),
	}})

	releaseBead, _ := store.Get(release.ID)
	verdicts := fidelityPriorStepVerdicts(store, releaseBead, nil)
	if len(verdicts) != 1 {
		t.Fatalf("prior_step_verdicts len = %d, want 1", len(verdicts))
	}
	m := verdicts[0].(map[string]any)
	if got := m["outcome"]; got != "revise" {
		t.Errorf("outcome = %v, want revise (gc.outcome=fail)", got)
	}
	if got := m["step_index"]; got != 0 {
		t.Errorf("step_index = %v, want 0", got)
	}
	if got := m["step_name"]; got != "code" {
		t.Errorf("step_name = %v, want code", got)
	}
}

// An empty gc.root_bead_id yields an empty (non-nil) verdict slice, not a panic.
func TestFidelityPriorStepVerdictsEmptyRootIsNonFatal(t *testing.T) {
	store := beads.NewMemStore()
	bead := beads.Bead{ID: "lone", Metadata: map[string]string{}}
	verdicts := fidelityPriorStepVerdicts(store, bead, nil)
	if verdicts == nil {
		t.Fatal("verdicts is nil, want empty slice")
	}
	if len(verdicts) != 0 {
		t.Errorf("verdicts len = %d, want 0", len(verdicts))
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

// A missing webhook URL must not panic: the production default URL is used and
// the script runs.
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
	if webhook["url"] != "https://ff-pipeline.koales.workers.dev/webhooks/gascity" {
		t.Errorf("webhook.url = %v, want production default URL", webhook["url"])
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
