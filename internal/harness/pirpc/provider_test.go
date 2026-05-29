package pirpc

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gastownhall/gascity/internal/harness"
)

func validRequest() harness.ExecutionRequest {
	return harness.ExecutionRequest{
		CityID:          "factory",
		SessionID:       "run-1",
		FormulaID:       "FORM-1",
		StepName:        "GeneratePatch",
		RoleName:        "Coder",
		Purpose:         "produce a patch",
		Inputs:          map[string]string{"SeedWorkspace": "tarball-ref"},
		DeclaredOutputs: []string{"Patch"},
		RuntimeConfig: map[string]any{
			"model_route":       "openrouter/moonshotai/kimi-k2",
			"max_repair_rounds": float64(1),
			"execution_surface": "rpc",
		},
		Policy: &harness.Policy{FilesystemScope: []string{"/workspace"}},
		ContextRefs: harness.ContextRefs{
			FnID: "FN-1", IsID: "IS-1", EsID: "ES-1", EpID: "EP-1",
		},
		VerifierContract: &harness.VerifierContract{
			DeclaredOutputMatch: []harness.OutputMatchRule{{Artifact: "Patch", Kind: "text"}},
		},
		IdempotencyKey: "run-1:GeneratePatch:0",
	}
}

// The provider satisfies the HarnessProvider interface.
func TestProvider_ImplementsHarnessProvider(t *testing.T) {
	var _ harness.HarnessProvider = (*Provider)(nil)
}

// AC-PI1: ExecuteStep translates the request to WorkerInput and POSTs to
// /__pi-container/execute; the field renames are applied at the boundary.
func TestExecuteStep_TranslatesRequestToWorkerInput(t *testing.T) {
	var captured map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/__pi-container/execute" {
			t.Errorf("expected /__pi-container/execute, got %q", r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &captured)
		writeContainerSuccess(w)
	}))
	defer srv.Close()

	p := mustNew(t, srv.URL)
	_, err := p.ExecuteStep(context.Background(), validRequest())
	if err != nil {
		t.Fatalf("ExecuteStep: %v", err)
	}

	if captured["stageName"] != "GeneratePatch" {
		t.Errorf("step_name → stageName failed: %v", captured["stageName"])
	}
	if captured["roleName"] != "Coder" {
		t.Errorf("role_name → roleName failed: %v", captured["roleName"])
	}
	if captured["runId"] != "run-1" {
		t.Errorf("session_id → runId failed: %v", captured["runId"])
	}
	ctxObj, _ := captured["context"].(map[string]any)
	inputArtifacts, _ := ctxObj["inputArtifacts"].(map[string]any)
	if inputArtifacts["SeedWorkspace"] != "tarball-ref" {
		t.Errorf("inputs → context.inputArtifacts failed: %v", inputArtifacts)
	}
	declared, _ := captured["declaredOutputs"].([]any)
	if len(declared) != 1 || declared[0] != "Patch" {
		t.Errorf("declared_outputs → declaredOutputs failed: %v", captured["declaredOutputs"])
	}
	if _, ok := captured["outputContracts"]; !ok {
		t.Errorf("verifier_contract → outputContracts missing")
	}
}

// AC-PI2 / AC-RS6: observation.totalUsage maps to model_usage.
func TestExecuteStep_MapsModelUsage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeContainerSuccess(w)
	}))
	defer srv.Close()

	p := mustNew(t, srv.URL)
	resp, err := p.ExecuteStep(context.Background(), validRequest())
	if err != nil {
		t.Fatalf("ExecuteStep: %v", err)
	}
	if resp.ModelUsage == nil {
		t.Fatalf("expected model_usage populated for pi-rpc (AC-RS6)")
	}
	if resp.ModelUsage.InputTokens != 100 || resp.ModelUsage.OutputTokens != 50 {
		t.Fatalf("unexpected token usage: %+v", resp.ModelUsage)
	}
	if resp.ModelUsage.ModelID == "" {
		t.Fatalf("expected model id in model_usage")
	}
}

// AC-RS5: runtime_identity carries provider_id and version; image digest comes
// from containerRuntime.
func TestExecuteStep_RuntimeIdentity(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeContainerSuccess(w)
	}))
	defer srv.Close()

	p := mustNew(t, srv.URL)
	resp, _ := p.ExecuteStep(context.Background(), validRequest())
	if resp.RuntimeIdentity.ProviderID != ProviderID {
		t.Fatalf("expected provider_id %q, got %q", ProviderID, resp.RuntimeIdentity.ProviderID)
	}
	if resp.RuntimeIdentity.Version == "" {
		t.Fatalf("expected non-empty version")
	}
}

// AC-PI2: contract-evaluation per artifact maps to artifact_manifest; a passing
// declared output is produced and status=completed.
func TestExecuteStep_BuildsManifestFromContractEvaluation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeContainerSuccess(w)
	}))
	defer srv.Close()

	p := mustNew(t, srv.URL)
	resp, _ := p.ExecuteStep(context.Background(), validRequest())
	if resp.Status != harness.StatusCompleted {
		t.Fatalf("expected completed, got %q (err=%+v)", resp.Status, resp.Error)
	}
	if !manifestHas(resp.ArtifactManifest, "Patch", harness.ManifestProduced) {
		t.Fatalf("expected Patch produced, got %+v", resp.ArtifactManifest)
	}
}

// AC-FC5: when a declared output is not in the produced set, status=failed with
// declared_outputs_missing and the self-report flag is set.
func TestExecuteStep_FailsWhenDeclaredOutputMissing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Container claims completion but produces nothing.
		_ = json.NewEncoder(w).Encode(map[string]any{
			"artifacts":        []string{},
			"artifactContents": map[string]string{},
			"message":          "GeneratePatch complete",
			"observation": map[string]any{
				"totalUsage":       map[string]any{"inputTokens": 10, "outputTokens": 5},
				"model":            map[string]any{"id": "openrouter/moonshotai/kimi-k2"},
				"containerRuntime": map[string]any{"workerVersionId": "v1"},
				"events":           []any{},
			},
		})
	}))
	defer srv.Close()

	p := mustNew(t, srv.URL)
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
}

// AC-PI3 / AC-FC4: a path-guard block in the container response becomes a
// policy_violation with a policy_events entry.
func TestExecuteStep_PathGuardBecomesPolicyViolation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]any{
				"code":    "PI_PATH_GUARD_BLOCKED",
				"message": "blocked path /etc/passwd",
			},
			"observation": map[string]any{
				"containerRuntime": map[string]any{"workerVersionId": "v1"},
				"events":           []any{},
			},
		})
	}))
	defer srv.Close()

	p := mustNew(t, srv.URL)
	resp, _ := p.ExecuteStep(context.Background(), validRequest())
	if resp.Status != harness.StatusPolicyViolation {
		t.Fatalf("expected policy_violation, got %q", resp.Status)
	}
	if len(resp.PolicyEvents) == 0 {
		t.Fatalf("expected a policy_events entry for the path-guard block (AC-PI3)")
	}
	found := false
	for _, e := range resp.PolicyEvents {
		if e.Kind == harness.PolicyViolation && e.Rule == "filesystem_scope" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a filesystem_scope violation event, got %+v", resp.PolicyEvents)
	}
}

// AC-FC6: a container timeout maps to status=timeout.
func TestExecuteStep_ContainerTimeoutMapsToTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusGatewayTimeout)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]any{
				"code":    "PI_CONTAINER_EXECUTE_TIMEOUT",
				"message": "pi container execution exceeded timeout",
			},
		})
	}))
	defer srv.Close()

	p := mustNew(t, srv.URL)
	resp, _ := p.ExecuteStep(context.Background(), validRequest())
	if resp.Status != harness.StatusTimeout {
		t.Fatalf("expected timeout, got %q", resp.Status)
	}
}

// AC-FC2: a request with no verifier contract is rejected before any HTTP call.
func TestExecuteStep_RejectsMissingVerifierContract(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		writeContainerSuccess(w)
	}))
	defer srv.Close()

	p := mustNew(t, srv.URL)
	req := validRequest()
	req.VerifierContract = nil
	resp, _ := p.ExecuteStep(context.Background(), req)
	if called {
		t.Fatalf("provider must reject before calling the container (AC-FC2)")
	}
	if resp.Error == nil || resp.Error.Code != "missing_verifier_contract" {
		t.Fatalf("expected missing_verifier_contract, got %+v", resp.Error)
	}
}

// AC-LC9: Status maps to GET /__pi-container/status.
func TestStatus_QueriesContainerStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/__pi-container/status" && r.Method == http.MethodGet {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"running":        true,
				"desiredBuildId": "b1",
				"startedBuildId": "b1",
				"queueDepth":     0,
			})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	p := mustNew(t, srv.URL)
	st, err := p.Status(context.Background(), harness.StatusRequest{})
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if !st.Healthy || !st.Ready {
		t.Fatalf("expected healthy+ready, got %+v", st)
	}
	if st.Capacity <= 0 {
		t.Fatalf("expected positive capacity when running, got %d", st.Capacity)
	}
}

func TestStatus_ReportsCapacityWhenContainerStopped(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/__pi-container/status" && r.Method == http.MethodGet {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"running":        false,
				"desiredBuildId": "b1",
				"startedBuildId": "",
				"queueDepth":     0,
			})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	p := mustNew(t, srv.URL)
	st, err := p.Status(context.Background(), harness.StatusRequest{})
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st.Capacity <= 0 {
		t.Fatalf("expected dispatch capacity while container is stopped, got %d", st.Capacity)
	}
}

// AC-LC6: CollectPolicyEvents returns a non-nil slice.
func TestCollectPolicyEvents_NeverNil(t *testing.T) {
	p := mustNew(t, "http://example.invalid")
	resp, err := p.CollectPolicyEvents(context.Background(), harness.CollectPolicyEventsRequest{SessionHandle: "run-1"})
	if err != nil {
		t.Fatalf("CollectPolicyEvents: %v", err)
	}
	if resp.PolicyEvents == nil {
		t.Fatalf("policy_events MUST be non-nil (AC-LC6)")
	}
}

// AC-LC7/AC-LC8: pi-rpc supports session archive (snapshot maps to it); restore
// is reported per spec.
func TestSnapshot_SupportedViaSessionArchive(t *testing.T) {
	p := mustNew(t, "http://example.invalid")
	snap, err := p.Snapshot(context.Background(), harness.SnapshotRequest{SessionHandle: "run-1"})
	if err != nil {
		t.Fatalf("Snapshot MUST NOT error: %v", err)
	}
	// pi-rpc captures a session archive; snapshot is not unsupported.
	if snap.Unsupported {
		t.Fatalf("pi-rpc supports session archive; snapshot must not be unsupported (AC-LC7)")
	}
}

// CapabilityDeclaration covers all eight slots and the declared pi-rpc keys
// (AC-REG5, AC-HT1), including ai_reasoning.
func TestCapabilityDeclaration(t *testing.T) {
	decl := CapabilityDeclaration()
	if len(decl.HarnessSlots) != 8 {
		t.Fatalf("expected 8 harness slots, got %d", len(decl.HarnessSlots))
	}
	hasAI := false
	for _, k := range decl.CapabilityKeys {
		if k == "ai_reasoning" {
			hasAI = true
		}
	}
	if !hasAI {
		t.Fatalf("pi-rpc MUST declare ai_reasoning (AC-HT1)")
	}
}

func mustNew(t *testing.T, url string) *Provider {
	t.Helper()
	p, err := New(Config{URL: url, Token: "test-token"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p
}

// writeContainerSuccess writes a representative successful ContainerExecuteResponse
// matching server.mjs::handleExecute, with Patch produced.
func writeContainerSuccess(w http.ResponseWriter) {
	_ = json.NewEncoder(w).Encode(map[string]any{
		"artifacts":        []string{"Patch"},
		"artifactContents": map[string]string{"Patch": "diff --git ..."},
		"message":          "GeneratePatch complete",
		"sessionArchive":   map[string]any{"bytes": 2048},
		"observation": map[string]any{
			"runId":            "run-1",
			"stageName":        "GeneratePatch",
			"totalUsage":       map[string]any{"inputTokens": 100, "outputTokens": 50},
			"model":            map[string]any{"id": "openrouter/moonshotai/kimi-k2"},
			"containerRuntime": map[string]any{"workerVersionId": "wv-123"},
			"events": []any{
				map[string]any{"type": "pi.spawned"},
			},
			"contractEvaluation": map[string]any{
				"findings": []any{
					map[string]any{"artifact": "Patch", "status": "pass"},
				},
			},
		},
	})
}

func manifestHas(manifest []harness.ManifestEntry, artifact string, status harness.ManifestStatus) bool {
	for _, e := range manifest {
		if e.Artifact == artifact && e.Status == status {
			return true
		}
	}
	return false
}
