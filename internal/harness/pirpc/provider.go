// Package pirpc implements the pi-rpc HarnessProvider: the runtime that
// produces real code. It translates the Gas City execution request envelope
// (AC-RQ1) into a WorkerInput and POSTs it to the ff-pipeline Worker route
// POST /__pi-container/execute, then translates the container response back into
// the response envelope (AC-RS1) per the IS §pi-rpc migration path (AC-PI1..7).
//
// pi-rpc covers all eight Harness Tuple slots (AC-HT1) and declares ai_reasoning.
// It performs model inference, so model_usage is populated (AC-RS6). It supports
// a session archive, which is the snapshot primitive (AC-LC7).
package pirpc

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/harness"
)

// ProviderID is the stable registry id for this provider.
const ProviderID = "pi-rpc"

const (
	defaultExecuteTimeout = 6 * time.Minute
	defaultStatusTimeout  = 30 * time.Second
	maxResponseBytes      = 8 << 20

	executePath = "/__pi-container/execute"
	statusPath  = "/__pi-container/status"
	restartPath = "/__pi-container/restart"
)

// Config configures a pi-rpc HarnessProvider.
type Config struct {
	// URL is the ff-pipeline Worker base URL (e.g.
	// https://ff-pipeline.koales.workers.dev). The provider calls its
	// /__pi-container/* routes. Required.
	URL string
	// Token is the bearer token sent on each call (the existing FF_API_KEY
	// pattern). Optional.
	Token string
	// Version is the provider version reported in runtime_identity (AC-RS5).
	Version string
	// Client is the HTTP client. Defaults to a client with no client-level
	// timeout; per-request context deadlines govern.
	Client *http.Client
	// ExecuteTimeout bounds an ExecuteStep call (AC-FC6).
	ExecuteTimeout time.Duration
}

// Provider is the pi-rpc HarnessProvider.
type Provider struct {
	base           *url.URL
	token          string
	version        string
	client         *http.Client
	executeTimeout time.Duration
}

var _ harness.HarnessProvider = (*Provider)(nil)

// New constructs a pi-rpc HarnessProvider.
func New(cfg Config) (*Provider, error) {
	raw := strings.TrimSpace(cfg.URL)
	if raw == "" {
		return nil, fmt.Errorf("pi-rpc: URL is required")
	}
	base, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("pi-rpc: parse URL: %w", err)
	}
	if base.Scheme == "" || base.Host == "" {
		return nil, fmt.Errorf("pi-rpc: URL must be absolute")
	}
	version := cfg.Version
	if version == "" {
		version = "unknown"
	}
	client := cfg.Client
	if client == nil {
		client = &http.Client{}
	}
	executeTimeout := cfg.ExecuteTimeout
	if executeTimeout <= 0 {
		executeTimeout = defaultExecuteTimeout
	}
	return &Provider{
		base:           base,
		token:          cfg.Token,
		version:        version,
		client:         client,
		executeTimeout: executeTimeout,
	}, nil
}

// CapabilityDeclaration returns the pi-rpc capability declaration (AC-REG5,
// AC-HT1). All eight slots; ai_reasoning plus the pi-rpc keys.
func CapabilityDeclaration() harness.CapabilityDeclaration {
	return harness.CapabilityDeclaration{
		HarnessSlots:   append([]string(nil), harness.HarnessSlots...),
		CapabilityKeys: []string{"ai_reasoning", "model_routing", "workspace_write_scope", "contract_evaluation", "tool_capability_probe", "session_archive"},
	}
}

// workerInput is the WorkerInput shape the container expects (server.mjs).
type workerInput struct {
	StageName        string           `json:"stageName"`
	RoleName         string           `json:"roleName"`
	RunID            string           `json:"runId"`
	Context          workerContext    `json:"context"`
	DeclaredOutputs  []string         `json:"declaredOutputs"`
	OutputContracts  []workerContract `json:"outputContracts,omitempty"`
	Model            *workerModel     `json:"model,omitempty"`
	ModelCandidates  []string         `json:"modelCandidates,omitempty"`
	MaxRepairRounds  *int             `json:"maxRepairRounds,omitempty"`
	ExecutionSurface string           `json:"executionSurface,omitempty"`
}

type workerContext struct {
	InputArtifacts map[string]string `json:"inputArtifacts"`
	TaskText       string            `json:"taskText,omitempty"`
	// Lineage pointers carried as opaque purpose-binding (AC-RQ2).
	ContextRefs map[string]string `json:"contextRefs,omitempty"`
}

type workerModel struct {
	ID string `json:"id"`
}

type workerContract struct {
	Artifact string `json:"artifact"`
	Kind     string `json:"kind,omitempty"`
}

// containerResponse is the ContainerExecuteResponse shape (server.mjs).
type containerResponse struct {
	Artifacts        []string          `json:"artifacts"`
	ArtifactContents map[string]string `json:"artifactContents"`
	Message          string            `json:"message"`
	SessionArchive   *struct {
		Bytes int64 `json:"bytes"`
	} `json:"sessionArchive"`
	Observation *containerObservation `json:"observation"`
	Error       *struct {
		Code       string `json:"code"`
		Message    string `json:"message"`
		StderrTail string `json:"stderrTail"`
	} `json:"error"`
}

type containerObservation struct {
	RunID      string `json:"runId"`
	StageName  string `json:"stageName"`
	TotalUsage *struct {
		InputTokens  int `json:"inputTokens"`
		OutputTokens int `json:"outputTokens"`
	} `json:"totalUsage"`
	Model *struct {
		ID string `json:"id"`
	} `json:"model"`
	ContainerRuntime   map[string]any   `json:"containerRuntime"`
	Events             []map[string]any `json:"events"`
	StderrTail         string           `json:"stderrTail"`
	ContractEvaluation *struct {
		Findings []struct {
			Artifact string `json:"artifact"`
			Status   string `json:"status"`
		} `json:"findings"`
	} `json:"contractEvaluation"`
}

// CreateSession ensures the container is ready by probing its status (AC-LC1).
// PI_CONTAINER is a warm bounded singleton DO (Open Question 2), so creating a
// session is a readiness check rather than an allocation.
func (p *Provider) CreateSession(ctx context.Context, req harness.CreateSessionRequest) (harness.CreateSessionResponse, error) {
	st, err := p.Status(ctx, harness.StatusRequest{})
	if err != nil {
		return harness.CreateSessionResponse{Status: "failed"}, fmt.Errorf("pi-rpc: create session: %w", err)
	}
	if !st.Ready {
		return harness.CreateSessionResponse{Status: "not_ready"}, fmt.Errorf("pi-rpc: container not ready")
	}
	handle := req.SessionID
	if handle == "" {
		handle = req.MoleculeID
	}
	return harness.CreateSessionResponse{SessionHandle: handle, Status: "ok"}, nil
}

// PrepareWorkspace is a no-op for pi-rpc: input materialization happens inside
// the container at execute time (SeedWorkspace prep, input-artifact write —
// AC-PI4). It validates the policy precondition (AC-FC3).
func (p *Provider) PrepareWorkspace(_ context.Context, req harness.PrepareWorkspaceRequest) (harness.PrepareWorkspaceResponse, error) {
	if req.Policy.IsEmpty() {
		return harness.PrepareWorkspaceResponse{Status: "failed"}, fmt.Errorf("pi-rpc: policy is required (AC-FC3)")
	}
	return harness.PrepareWorkspaceResponse{Status: "ok"}, nil
}

// ExecuteStep translates the request to WorkerInput, POSTs to the execute route,
// and translates the response back to the response envelope (AC-LC3, AC-PI1).
func (p *Provider) ExecuteStep(ctx context.Context, req harness.ExecutionRequest) (harness.ExecutionResponse, error) {
	resp := harness.ExecutionResponse{
		PolicyEvents:     []harness.PolicyEvent{}, // never nil (AC-LC6)
		RuntimeIdentity:  harness.RuntimeIdentity{ProviderID: ProviderID, Version: p.version},
		ArtifactManifest: manifestAllMissing(req.DeclaredOutputs),
	}

	// Defense-in-depth preconditions (AC-FC2, AC-FC3): reject before any call.
	if req.VerifierContract.IsEmpty() {
		resp.Status = harness.StatusFailed
		resp.ProviderVerdict = harness.ProviderVerdict{Status: harness.StatusFailed, Reason: "missing verifier contract"}
		resp.Error = &harness.ProviderError{Code: "missing_verifier_contract", Message: "verifier_contract is mandatory (AC-FC2)"}
		return resp, nil
	}
	if req.Policy.IsEmpty() {
		resp.Status = harness.StatusFailed
		resp.ProviderVerdict = harness.ProviderVerdict{Status: harness.StatusFailed, Reason: "missing policy"}
		resp.Error = &harness.ProviderError{Code: "missing_policy", Message: "policy is mandatory (AC-FC3)"}
		return resp, nil
	}

	input := translateRequest(req)
	status, body, err := p.post(ctx, executePath, input, p.executeTimeout)
	if err != nil {
		resp.Status = harness.StatusFailed
		resp.ProviderVerdict = harness.ProviderVerdict{Status: harness.StatusFailed, Reason: "transport error"}
		resp.Error = &harness.ProviderError{Code: "transport_error", Message: err.Error()}
		return resp, nil
	}

	var cr containerResponse
	if jsonErr := json.Unmarshal(body, &cr); jsonErr != nil {
		resp.Status = harness.StatusFailed
		resp.ProviderVerdict = harness.ProviderVerdict{Status: harness.StatusFailed, Reason: "malformed response"}
		resp.Error = &harness.ProviderError{Code: "malformed_response", Message: jsonErr.Error()}
		return resp, nil
	}

	return p.translateResponse(req, status, &cr), nil
}

// CollectArtifacts is satisfied inline by ExecuteStep for pi-rpc (AC-PI6); when
// called standalone it returns an all-missing manifest because the container
// returns artifacts only at execute time.
func (p *Provider) CollectArtifacts(_ context.Context, req harness.CollectArtifactsRequest) (harness.CollectArtifactsResponse, error) {
	return harness.CollectArtifactsResponse{
		Artifacts:        nil,
		ArtifactManifest: manifestAllMissing(req.DeclaredOutputs),
	}, nil
}

// CollectLogs returns references; the pi observation event stream and stderr
// ring buffer are returned inline in ExecuteStep (AC-LC5, AC-PI2).
func (p *Provider) CollectLogs(_ context.Context, req harness.CollectLogsRequest) (harness.CollectLogsResponse, error) {
	return harness.CollectLogsResponse{StdoutRef: "run:" + req.SessionHandle}, nil
}

// CollectPolicyEvents returns a non-nil slice (AC-LC6). Path-guard blocks are
// surfaced inline in ExecuteStep (AC-PI3); standalone there is nothing to return.
func (p *Provider) CollectPolicyEvents(_ context.Context, _ harness.CollectPolicyEventsRequest) (harness.CollectPolicyEventsResponse, error) {
	return harness.CollectPolicyEventsResponse{PolicyEvents: []harness.PolicyEvent{}}, nil
}

// Snapshot maps to the container session-archive capture (AC-LC7). pi-rpc
// supports snapshots; the archive ref is produced at execute time and stored by
// Gas City. Standalone this reports support without a ref.
func (p *Provider) Snapshot(_ context.Context, _ harness.SnapshotRequest) (harness.SnapshotResponse, error) {
	return harness.SnapshotResponse{Unsupported: false}, nil
}

// Restore is reported as supported for pi-rpc session continuation (AC-LC8).
func (p *Provider) Restore(_ context.Context, _ harness.RestoreRequest) (harness.RestoreResponse, error) {
	return harness.RestoreResponse{Status: "ok"}, nil
}

// Status maps to GET /__pi-container/status (AC-LC9). A reachable PI Worker has
// dispatch capacity even when the bounded singleton Container is currently
// stopped: /__pi-container/execute is the operation that wakes it.
func (p *Provider) Status(ctx context.Context, _ harness.StatusRequest) (harness.StatusResponse, error) {
	_, body, err := p.get(ctx, statusPath, defaultStatusTimeout)
	if err != nil {
		return harness.StatusResponse{ProviderVersion: p.version}, fmt.Errorf("pi-rpc: status: %w", err)
	}
	var cs struct {
		Running        bool   `json:"running"`
		DesiredBuildID string `json:"desiredBuildId"`
		StartedBuildID string `json:"startedBuildId"`
		QueueDepth     int    `json:"queueDepth"`
	}
	if jsonErr := json.Unmarshal(body, &cs); jsonErr != nil {
		return harness.StatusResponse{ProviderVersion: p.version}, fmt.Errorf("pi-rpc: decode status: %w", jsonErr)
	}
	st := harness.StatusResponse{
		Healthy:         cs.Running,
		Ready:           cs.Running && cs.QueueDepth == 0 || cs.Running,
		ProviderVersion: p.version,
		Capacity:        1,
	}
	return st, nil
}

// Restart maps to POST /__pi-container/restart (AC-LC10).
func (p *Provider) Restart(ctx context.Context, _ harness.RestartRequest) (harness.RestartResponse, error) {
	status, _, err := p.post(ctx, restartPath, struct{}{}, defaultStatusTimeout)
	if err != nil {
		return harness.RestartResponse{Status: "failed"}, fmt.Errorf("pi-rpc: restart: %w", err)
	}
	if status < 200 || status >= 300 {
		return harness.RestartResponse{Status: "failed"}, fmt.Errorf("pi-rpc: restart status %d", status)
	}
	return harness.RestartResponse{Status: "ok"}, nil
}

// Destroy is a no-op for the warm bounded singleton (Open Question 2): the
// container is not torn down per Gas City session. Scoped state lives in the DO.
func (p *Provider) Destroy(_ context.Context, _ harness.DestroyRequest) (harness.DestroyResponse, error) {
	return harness.DestroyResponse{Status: "ok"}, nil
}

// translateRequest applies the AC-PI1 field renames at the envelope boundary.
func translateRequest(req harness.ExecutionRequest) workerInput {
	in := workerInput{
		StageName:       req.StepName,
		RoleName:        req.RoleName,
		RunID:           req.SessionID,
		DeclaredOutputs: req.DeclaredOutputs,
		Context: workerContext{
			InputArtifacts: req.Inputs,
			TaskText:       strings.TrimSpace(req.Purpose),
			ContextRefs: map[string]string{
				"fn_id": req.ContextRefs.FnID,
				"is_id": req.ContextRefs.IsID,
				"es_id": req.ContextRefs.EsID,
				"ep_id": req.ContextRefs.EpID,
			},
		},
	}
	if in.Context.InputArtifacts == nil {
		in.Context.InputArtifacts = map[string]string{}
	}
	// verifier_contract → outputContracts (AC-PI1).
	if req.VerifierContract != nil {
		for _, rule := range req.VerifierContract.DeclaredOutputMatch {
			in.OutputContracts = append(in.OutputContracts, workerContract{Artifact: rule.Artifact, Kind: rule.Kind})
		}
	}
	// runtime_config → model route + candidates, repair budget, surface (AC-PI4).
	if v, ok := stringField(req.RuntimeConfig, "model_route"); ok {
		in.Model = &workerModel{ID: v}
	}
	in.ModelCandidates = stringSliceField(req.RuntimeConfig, "model_candidates")
	if n, ok := intField(req.RuntimeConfig, "max_repair_rounds"); ok {
		in.MaxRepairRounds = &n
	}
	if v, ok := stringField(req.RuntimeConfig, "execution_surface"); ok {
		in.ExecutionSurface = v
	}
	return in
}

// translateResponse decomposes the container observation into envelope fields
// (AC-PI2) and applies the fail-closed stop-condition rules (AC-FC4, AC-FC5,
// AC-FC6).
func (p *Provider) translateResponse(req harness.ExecutionRequest, httpStatus int, cr *containerResponse) harness.ExecutionResponse {
	resp := harness.ExecutionResponse{
		PolicyEvents:    []harness.PolicyEvent{},
		RuntimeIdentity: harness.RuntimeIdentity{ProviderID: ProviderID, Version: p.version},
	}

	// runtime_identity image digest from containerRuntime (AC-RS5).
	if cr.Observation != nil {
		resp.RuntimeIdentity.ImageDigest = imageDigestFromRuntime(cr.Observation.ContainerRuntime)
		resp.ModelUsage = modelUsageFromObservation(cr.Observation)
		resp.Logs = logsFromObservation(cr.Observation)
		resp.StepOutputs = map[string]any{"events": cr.Observation.Events}
	}

	// session_archive_ref (AC-RS7): Gas City stores the bytes; the ref is owned
	// by Gas City. Here we surface that an archive was produced.
	if cr.SessionArchive != nil {
		resp.SessionArchiveRef = "session-archive:" + req.SessionID
	}

	// Error path: classify the structured error into the right status.
	if cr.Error != nil {
		code := cr.Error.Code
		switch {
		case code == "PI_PATH_GUARD_BLOCKED" || code == "PI_SEED_WORKSPACE_PATCH_GUARD":
			// AC-PI3 / AC-FC4: path-guard block → policy_violation.
			resp.Status = harness.StatusPolicyViolation
			resp.ProviderVerdict = harness.ProviderVerdict{Status: harness.StatusPolicyViolation, Reason: "filesystem scope violation"}
			resp.PolicyEvents = append(resp.PolicyEvents, harness.PolicyEvent{
				Kind: harness.PolicyViolation,
				Rule: "filesystem_scope",
				Path: pathFromMessage(cr.Error.Message),
			})
			resp.Error = &harness.ProviderError{Code: "policy_violation", Message: cr.Error.Message}
		case code == "PI_CONTAINER_EXECUTE_TIMEOUT" || code == "container_execute_timed_out" || httpStatus == http.StatusGatewayTimeout:
			// AC-FC6: timeout.
			resp.Status = harness.StatusTimeout
			resp.ProviderVerdict = harness.ProviderVerdict{Status: harness.StatusTimeout, Reason: "execution deadline exceeded"}
			resp.Error = &harness.ProviderError{Code: "execution_timeout", Message: cr.Error.Message}
		default:
			resp.Status = harness.StatusFailed
			resp.ProviderVerdict = harness.ProviderVerdict{Status: harness.StatusFailed, Reason: code}
			resp.Error = &harness.ProviderError{Code: lowerCode(code), Message: cr.Error.Message}
		}
		resp.ArtifactManifest = manifestAllMissing(req.DeclaredOutputs)
		return resp
	}

	// Success path: build the manifest from the produced set and contract
	// evaluation (AC-PI2, AC-RS3).
	resp.Artifacts = artifactsFromContents(cr.ArtifactContents)
	resp.ArtifactManifest = buildManifest(req.DeclaredOutputs, cr.Artifacts)

	if missing := missingOutputs(resp.ArtifactManifest); len(missing) > 0 {
		// AC-FC5: "agent says it is done" with declared outputs missing is not a
		// valid completion.
		resp.Status = harness.StatusFailed
		resp.ProviderVerdict = harness.ProviderVerdict{Status: harness.StatusFailed, Reason: "declared outputs missing"}
		resp.Error = &harness.ProviderError{Code: "declared_outputs_missing", Message: fmt.Sprintf("missing: %v", missing)}
		resp.CompletionClaimedWithoutManifest = true
		return resp
	}

	resp.Status = harness.StatusCompleted
	resp.ProviderVerdict = harness.ProviderVerdict{Status: harness.StatusCompleted, Reason: "all declared outputs produced"}
	return resp
}

// --- HTTP helpers ---

func (p *Provider) post(ctx context.Context, path string, body any, timeout time.Duration) (int, []byte, error) {
	data, err := json.Marshal(body)
	if err != nil {
		return 0, nil, fmt.Errorf("marshal: %w", err)
	}
	return p.do(ctx, http.MethodPost, path, bytes.NewReader(data), timeout)
}

func (p *Provider) get(ctx context.Context, path string, timeout time.Duration) (int, []byte, error) {
	return p.do(ctx, http.MethodGet, path, nil, timeout)
}

func (p *Provider) do(parent context.Context, method, path string, body io.Reader, timeout time.Duration) (int, []byte, error) {
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()

	target := *p.base
	target.Path = strings.TrimRight(target.Path, "/") + path
	req, err := http.NewRequestWithContext(ctx, method, target.String(), body)
	if err != nil {
		return 0, nil, fmt.Errorf("build request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	if p.token != "" {
		req.Header.Set("Authorization", "Bearer "+p.token)
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("request: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return resp.StatusCode, nil, fmt.Errorf("read response: %w", err)
	}
	return resp.StatusCode, data, nil
}

// --- translation helpers ---

func modelUsageFromObservation(obs *containerObservation) *harness.ModelUsage {
	if obs == nil || obs.TotalUsage == nil {
		return nil
	}
	mu := &harness.ModelUsage{
		InputTokens:  obs.TotalUsage.InputTokens,
		OutputTokens: obs.TotalUsage.OutputTokens,
	}
	if obs.Model != nil {
		mu.ModelID = obs.Model.ID
	}
	return mu
}

func logsFromObservation(obs *containerObservation) harness.Logs {
	logs := harness.Logs{}
	if obs == nil {
		return logs
	}
	if obs.StderrTail != "" {
		logs.StderrRef = "stderr-tail"
	}
	for _, ev := range obs.Events {
		t, _ := ev["type"].(string)
		ts, _ := ev["ts"].(string)
		if t == "" {
			continue
		}
		logs.TraceSpans = append(logs.TraceSpans, harness.TraceSpan{Timestamp: ts, Type: t})
	}
	return logs
}

func imageDigestFromRuntime(rt map[string]any) string {
	if rt == nil {
		return ""
	}
	if v, ok := rt["workerVersionId"].(string); ok {
		return v
	}
	return ""
}

func artifactsFromContents(contents map[string]string) []harness.Artifact {
	if len(contents) == 0 {
		return nil
	}
	arts := make([]harness.Artifact, 0, len(contents))
	for path, content := range contents {
		arts = append(arts, harness.Artifact{
			Path:     path,
			Size:     int64(len(content)),
			Checksum: checksum(content),
		})
	}
	return arts
}

func buildManifest(declared []string, produced []string) []harness.ManifestEntry {
	producedSet := make(map[string]struct{}, len(produced))
	for _, name := range produced {
		producedSet[name] = struct{}{}
	}
	declaredSet := make(map[string]struct{}, len(declared))
	manifest := make([]harness.ManifestEntry, 0, len(declared)+len(produced))
	for _, name := range declared {
		declaredSet[name] = struct{}{}
		status := harness.ManifestMissing
		if _, ok := producedSet[name]; ok {
			status = harness.ManifestProduced
		}
		manifest = append(manifest, harness.ManifestEntry{Artifact: name, Status: status})
	}
	for _, name := range produced {
		if _, ok := declaredSet[name]; !ok {
			manifest = append(manifest, harness.ManifestEntry{Artifact: name, Status: harness.ManifestExtra})
		}
	}
	return manifest
}

func manifestAllMissing(declared []string) []harness.ManifestEntry {
	manifest := make([]harness.ManifestEntry, 0, len(declared))
	for _, name := range declared {
		manifest = append(manifest, harness.ManifestEntry{Artifact: name, Status: harness.ManifestMissing})
	}
	return manifest
}

func missingOutputs(manifest []harness.ManifestEntry) []string {
	var missing []string
	for _, e := range manifest {
		if e.Status == harness.ManifestMissing {
			missing = append(missing, e.Artifact)
		}
	}
	return missing
}

func stringField(m map[string]any, key string) (string, bool) {
	if m == nil {
		return "", false
	}
	v, ok := m[key].(string)
	return v, ok && v != ""
}

func stringSliceField(m map[string]any, key string) []string {
	if m == nil {
		return nil
	}
	switch v := m[key].(type) {
	case []string:
		return v
	case []any:
		out := make([]string, 0, len(v))
		for _, item := range v {
			if s, ok := item.(string); ok {
				out = append(out, s)
			}
		}
		return out
	default:
		return nil
	}
}

func intField(m map[string]any, key string) (int, bool) {
	if m == nil {
		return 0, false
	}
	switch v := m[key].(type) {
	case int:
		return v, true
	case int64:
		return int(v), true
	case float64:
		return int(v), true
	default:
		return 0, false
	}
}

func pathFromMessage(msg string) string {
	// Best-effort: server.mjs path-guard messages contain the blocked path.
	idx := strings.Index(msg, "/")
	if idx < 0 {
		return ""
	}
	return strings.TrimSpace(msg[idx:])
}

func lowerCode(code string) string {
	return strings.ToLower(code)
}

func checksum(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}
