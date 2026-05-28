package cloudflare

import (
	"context"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/harness"
	"github.com/gastownhall/gascity/internal/runtime"
)

func newTestProvider(t *testing.T, fake *runtime.Fake) *Provider {
	t.Helper()
	p, err := New(Config{
		Runtime:      fake,
		Version:      "test-1",
		PollInterval: time.Millisecond,
		PollTimeout:  50 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p
}

func validRequest() harness.ExecutionRequest {
	return harness.ExecutionRequest{
		CityID:          "factory",
		SessionID:       "sess-1",
		FormulaID:       "FORM-1",
		StepName:        "setup",
		RoleName:        "coder",
		Purpose:         "prepare workspace",
		Inputs:          map[string]string{},
		DeclaredOutputs: []string{"out.txt"},
		RuntimeConfig:   map[string]any{"command": "echo hi"},
		Policy:          &harness.Policy{FilesystemScope: []string{"/workspace"}},
		VerifierContract: &harness.VerifierContract{
			DeclaredOutputMatch: []harness.OutputMatchRule{{Artifact: "out.txt", Kind: "text"}},
		},
		IdempotencyKey: "sess-1:setup:0",
	}
}

// The provider satisfies the HarnessProvider interface.
func TestProvider_ImplementsHarnessProvider(t *testing.T) {
	var _ harness.HarnessProvider = (*Provider)(nil)
}

// AC-LC1: CreateSession maps to Start; readiness via IsRunning.
func TestCreateSession_StartsRuntime(t *testing.T) {
	fake := runtime.NewFake()
	p := newTestProvider(t, fake)

	resp, err := p.CreateSession(context.Background(), harness.CreateSessionRequest{
		CityID: "factory", SessionID: "sess-1", FormulaID: "FORM-1",
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if resp.SessionHandle == "" {
		t.Fatalf("expected a session handle")
	}
	if !fake.IsRunning(resp.SessionHandle) {
		t.Fatalf("expected the underlying session to be running")
	}
}

// AC-LC1 fail-closed: a runtime that cannot start returns non-OK.
func TestCreateSession_FailClosedOnStartError(t *testing.T) {
	fake := runtime.NewFailFake()
	p := newTestProvider(t, fake)

	_, err := p.CreateSession(context.Background(), harness.CreateSessionRequest{SessionID: "sess-1"})
	if err == nil {
		t.Fatalf("expected CreateSession to fail closed when the runtime cannot start")
	}
}

// AC-FC2: a request with no verifier contract is rejected (defense in depth).
func TestExecuteStep_RejectsMissingVerifierContract(t *testing.T) {
	fake := runtime.NewFake()
	p := newTestProvider(t, fake)
	mustCreate(t, p, fake)

	req := validRequest()
	req.VerifierContract = nil
	resp, err := p.ExecuteStep(context.Background(), req)
	if err == nil && resp.Status == harness.StatusCompleted {
		t.Fatalf("expected rejection for missing verifier contract")
	}
	if resp.Error == nil || resp.Error.Code != "missing_verifier_contract" {
		t.Fatalf("expected missing_verifier_contract error, got %+v", resp.Error)
	}
}

// AC-FC3: a request with no policy is rejected (defense in depth).
func TestExecuteStep_RejectsMissingPolicy(t *testing.T) {
	fake := runtime.NewFake()
	p := newTestProvider(t, fake)
	mustCreate(t, p, fake)

	req := validRequest()
	req.Policy = nil
	resp, _ := p.ExecuteStep(context.Background(), req)
	if resp.Error == nil || resp.Error.Code != "missing_policy" {
		t.Fatalf("expected missing_policy error, got %+v", resp.Error)
	}
}

// AC-RS5: runtime_identity always carries provider_id and version.
func TestExecuteStep_AlwaysCarriesRuntimeIdentity(t *testing.T) {
	fake := runtime.NewFake()
	p := newTestProvider(t, fake)
	mustCreate(t, p, fake)

	resp, _ := p.ExecuteStep(context.Background(), validRequest())
	if resp.RuntimeIdentity.ProviderID != ProviderID {
		t.Fatalf("expected provider_id %q, got %q", ProviderID, resp.RuntimeIdentity.ProviderID)
	}
	if resp.RuntimeIdentity.Version == "" {
		t.Fatalf("expected a non-empty version")
	}
}

// AC-RS6: cloudflare-sandbox performs no inference, so model_usage is nil.
func TestExecuteStep_ModelUsageNil(t *testing.T) {
	fake := runtime.NewFake()
	p := newTestProvider(t, fake)
	mustCreate(t, p, fake)

	resp, _ := p.ExecuteStep(context.Background(), validRequest())
	if resp.ModelUsage != nil {
		t.Fatalf("expected nil model_usage for cloudflare-sandbox, got %+v", resp.ModelUsage)
	}
}

// AC-LC6: policy_events is never nil — an empty slice is contract-valid.
func TestExecuteStep_PolicyEventsNeverNil(t *testing.T) {
	fake := runtime.NewFake()
	p := newTestProvider(t, fake)
	mustCreate(t, p, fake)

	resp, _ := p.ExecuteStep(context.Background(), validRequest())
	if resp.PolicyEvents == nil {
		t.Fatalf("policy_events MUST be a non-nil slice (AC-LC6)")
	}
}

// AC-FC5: an end-of-turn signal with a declared output still missing yields
// status=failed with declared_outputs_missing, and the externally-verifiable
// stop-condition flag is set.
func TestExecuteStep_FailsWhenDeclaredOutputMissing(t *testing.T) {
	fake := runtime.NewFake()
	p := newTestProvider(t, fake)
	mustCreate(t, p, fake)

	// No produced artifacts configured → the declared output is missing.
	resp, _ := p.ExecuteStep(context.Background(), validRequest())
	if resp.Status != harness.StatusFailed {
		t.Fatalf("expected failed, got %q", resp.Status)
	}
	if resp.Error == nil || resp.Error.Code != "declared_outputs_missing" {
		t.Fatalf("expected declared_outputs_missing, got %+v", resp.Error)
	}
	if !resp.CompletionClaimedWithoutManifest {
		t.Fatalf("expected completion_claimed_without_manifest=true (AC-FC5)")
	}
	if !manifestHas(resp.ArtifactManifest, "out.txt", harness.ManifestMissing) {
		t.Fatalf("expected out.txt marked missing, got %+v", resp.ArtifactManifest)
	}
}

// AC-FC5 / AC-RS3: when every declared output is produced, status=completed.
func TestExecuteStep_CompletesWhenAllProduced(t *testing.T) {
	fake := runtime.NewFake()
	p := newTestProvider(t, fake)
	mustCreate(t, p, fake)

	p.produced = func(_ context.Context, _ string, declared []string) ([]harness.Artifact, error) {
		arts := make([]harness.Artifact, 0, len(declared))
		for _, name := range declared {
			arts = append(arts, harness.Artifact{Path: name, Size: 2, Checksum: "abc"})
		}
		return arts, nil
	}

	resp, err := p.ExecuteStep(context.Background(), validRequest())
	if err != nil {
		t.Fatalf("ExecuteStep: %v", err)
	}
	if resp.Status != harness.StatusCompleted {
		t.Fatalf("expected completed, got %q (err=%+v)", resp.Status, resp.Error)
	}
	if resp.CompletionClaimedWithoutManifest {
		t.Fatalf("manifest-backed completion must not set the self-report flag")
	}
	if !manifestHas(resp.ArtifactManifest, "out.txt", harness.ManifestProduced) {
		t.Fatalf("expected out.txt produced, got %+v", resp.ArtifactManifest)
	}
}

// AC-RS3: a produced artifact not in declared_outputs is marked extra and does
// not fail the provider.
func TestExecuteStep_ExtraArtifactDoesNotFail(t *testing.T) {
	fake := runtime.NewFake()
	p := newTestProvider(t, fake)
	mustCreate(t, p, fake)

	p.produced = func(_ context.Context, _ string, declared []string) ([]harness.Artifact, error) {
		return []harness.Artifact{
			{Path: "out.txt", Size: 1, Checksum: "a"},
			{Path: "scratch.tmp", Size: 1, Checksum: "b"},
		}, nil
	}

	resp, _ := p.ExecuteStep(context.Background(), validRequest())
	if resp.Status != harness.StatusCompleted {
		t.Fatalf("extra artifact must not fail completion, got %q", resp.Status)
	}
	if !manifestHas(resp.ArtifactManifest, "scratch.tmp", harness.ManifestExtra) {
		t.Fatalf("expected scratch.tmp marked extra, got %+v", resp.ArtifactManifest)
	}
}

// AC-LC7/AC-LC8: cloudflare-sandbox does not support snapshot/restore.
func TestSnapshotRestore_Unsupported(t *testing.T) {
	fake := runtime.NewFake()
	p := newTestProvider(t, fake)

	snap, err := p.Snapshot(context.Background(), harness.SnapshotRequest{SessionHandle: "s"})
	if err != nil {
		t.Fatalf("Snapshot MUST NOT error on unsupported (AC-LC7): %v", err)
	}
	if !snap.Unsupported {
		t.Fatalf("expected snapshot unsupported=true")
	}
	rest, err := p.Restore(context.Background(), harness.RestoreRequest{SessionHandle: "s", SnapshotRef: "x"})
	if err != nil {
		t.Fatalf("Restore MUST NOT error on unsupported (AC-LC8): %v", err)
	}
	if !rest.Unsupported {
		t.Fatalf("expected restore unsupported=true")
	}
}

// AC-LC9: Status reports readiness from the runtime.
func TestStatus_ReportsReady(t *testing.T) {
	fake := runtime.NewFake()
	p := newTestProvider(t, fake)
	handle := mustCreate(t, p, fake)

	st, err := p.Status(context.Background(), harness.StatusRequest{SessionHandle: handle})
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if !st.Healthy {
		t.Fatalf("expected healthy session")
	}
	if st.ProviderVersion == "" {
		t.Fatalf("expected provider version")
	}
}

// AC-LC10: Restart stops then starts the runtime.
func TestRestart_StopsThenStarts(t *testing.T) {
	fake := runtime.NewFake()
	p := newTestProvider(t, fake)
	handle := mustCreate(t, p, fake)

	resp, err := p.Restart(context.Background(), harness.RestartRequest{SessionHandle: handle, Reason: "stale"})
	if err != nil {
		t.Fatalf("Restart: %v", err)
	}
	if resp.Status != "ok" {
		t.Fatalf("expected ok restart status, got %q", resp.Status)
	}
	if !fake.IsRunning(handle) {
		t.Fatalf("expected the runtime running after restart")
	}
}

// AC-LC11: Destroy stops the runtime.
func TestDestroy_StopsRuntime(t *testing.T) {
	fake := runtime.NewFake()
	p := newTestProvider(t, fake)
	handle := mustCreate(t, p, fake)

	_, err := p.Destroy(context.Background(), harness.DestroyRequest{SessionHandle: handle})
	if err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if fake.IsRunning(handle) {
		t.Fatalf("expected the runtime stopped after destroy")
	}
}

// CapabilityDeclaration covers all eight slots and the declared cloudflare keys
// (AC-REG5, AC-HT2). It explicitly omits ai_reasoning (AC-HT3).
func TestCapabilityDeclaration(t *testing.T) {
	decl := CapabilityDeclaration()
	if len(decl.HarnessSlots) != 8 {
		t.Fatalf("expected 8 harness slots, got %d", len(decl.HarnessSlots))
	}
	for _, k := range decl.CapabilityKeys {
		if k == "ai_reasoning" {
			t.Fatalf("cloudflare-sandbox MUST NOT declare ai_reasoning (AC-HT3)")
		}
	}
}

func mustCreate(t *testing.T, p *Provider, _ *runtime.Fake) string {
	t.Helper()
	resp, err := p.CreateSession(context.Background(), harness.CreateSessionRequest{
		CityID: "factory", SessionID: "sess-1", FormulaID: "FORM-1",
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	return resp.SessionHandle
}

func manifestHas(manifest []harness.ManifestEntry, artifact string, status harness.ManifestStatus) bool {
	for _, e := range manifest {
		if e.Artifact == artifact && e.Status == status {
			return true
		}
	}
	return false
}
