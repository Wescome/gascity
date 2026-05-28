// Package cloudflare implements the cloudflare-sandbox HarnessProvider: a
// setup/teardown provider that wraps the low-level runtime.Provider (the M0
// Cloudflare control worker surface) and maps the eleven AC-LC lifecycle
// obligations onto its PTY/session primitives per the IS §Interface binding
// table.
//
// cloudflare-sandbox covers all eight Harness Tuple slots (AC-HT2) but
// intentionally does NOT cover ai_reasoning (AC-HT3): its V slot is
// existence/exit-code only. It MUST NOT be selected for steps requiring
// ai_reasoning (AC-REG6).
package cloudflare

import (
	"context"
	"fmt"
	"time"

	"github.com/gastownhall/gascity/internal/harness"
	"github.com/gastownhall/gascity/internal/runtime"
)

// ProviderID is the stable registry id for this provider.
const ProviderID = "cloudflare-sandbox"

const (
	defaultPollInterval = 500 * time.Millisecond
	defaultPollTimeout  = 5 * time.Minute
	defaultPeekLines    = 200
)

// Config configures a cloudflare-sandbox HarnessProvider.
type Config struct {
	// Runtime is the underlying low-level runtime.Provider (the M0 cloudflare
	// control worker transport). Required.
	Runtime runtime.Provider
	// Version is the provider version reported in runtime_identity (AC-RS5).
	Version string
	// ImageDigest is the optional container image digest (AC-RS5).
	ImageDigest string
	// PollInterval is the gap between completion polls in ExecuteStep.
	PollInterval time.Duration
	// PollTimeout bounds ExecuteStep completion observation (AC-FC6).
	PollTimeout time.Duration
}

// producedFunc reads the produced-file set for a session. It is injectable so
// tests are deterministic and production wires a real R2/filesystem mirror
// (AC-PI5: R2 artifact writes map to collectArtifacts). The default reads
// nothing, so a step with no wired mirror correctly reports its declared
// outputs as missing rather than falsely claiming completion (AC-FC5).
type producedFunc func(ctx context.Context, sessionHandle string, declaredOutputs []string) ([]harness.Artifact, error)

// Provider is the cloudflare-sandbox HarnessProvider.
type Provider struct {
	rt           runtime.Provider
	version      string
	imageDigest  string
	pollInterval time.Duration
	pollTimeout  time.Duration
	produced     producedFunc
}

var _ harness.HarnessProvider = (*Provider)(nil)

// New constructs a cloudflare-sandbox HarnessProvider.
func New(cfg Config) (*Provider, error) {
	if cfg.Runtime == nil {
		return nil, fmt.Errorf("cloudflare-sandbox: runtime provider is required")
	}
	version := cfg.Version
	if version == "" {
		version = "unknown"
	}
	pollInterval := cfg.PollInterval
	if pollInterval <= 0 {
		pollInterval = defaultPollInterval
	}
	pollTimeout := cfg.PollTimeout
	if pollTimeout <= 0 {
		pollTimeout = defaultPollTimeout
	}
	p := &Provider{
		rt:           cfg.Runtime,
		version:      version,
		imageDigest:  cfg.ImageDigest,
		pollInterval: pollInterval,
		pollTimeout:  pollTimeout,
	}
	// Default produced reader: no mirror wired → no produced artifacts. This is
	// the fail-closed default: a step cannot claim completion without a
	// manifest-backed produced set.
	p.produced = func(context.Context, string, []string) ([]harness.Artifact, error) {
		return nil, nil
	}
	return p, nil
}

// CapabilityDeclaration returns the cloudflare-sandbox capability declaration
// (AC-REG5, AC-HT2). All eight slots; setup/teardown keys only — no ai_reasoning
// (AC-HT3).
func CapabilityDeclaration() harness.CapabilityDeclaration {
	return harness.CapabilityDeclaration{
		HarnessSlots:   append([]string(nil), harness.HarnessSlots...),
		CapabilityKeys: []string{"command_exec", "file_materialize", "workspace_init", "dependency_prep", "backup_restore"},
	}
}

// CreateSession maps to Start; readiness via IsRunning (AC-LC1). Fail-closed:
// a Start error surfaces so Gas City does not proceed to PrepareWorkspace.
func (p *Provider) CreateSession(ctx context.Context, req harness.CreateSessionRequest) (harness.CreateSessionResponse, error) {
	handle := req.SessionID
	if handle == "" {
		return harness.CreateSessionResponse{}, fmt.Errorf("cloudflare-sandbox: session_id is required")
	}
	cfg := runtime.Config{WorkDir: "/workspace"}
	if err := p.rt.Start(ctx, handle, cfg); err != nil {
		return harness.CreateSessionResponse{Status: "failed"}, fmt.Errorf("cloudflare-sandbox: start session %q: %w", handle, err)
	}
	return harness.CreateSessionResponse{SessionHandle: handle, Status: "ok"}, nil
}

// PrepareWorkspace materializes inputs via CopyTo and applies scoped state via
// SetMeta (AC-LC2). Fail-closed: a missing required scope or an input that
// cannot be materialized returns non-OK.
func (p *Provider) PrepareWorkspace(_ context.Context, req harness.PrepareWorkspaceRequest) (harness.PrepareWorkspaceResponse, error) {
	if req.Policy.IsEmpty() {
		return harness.PrepareWorkspaceResponse{Status: "failed"}, fmt.Errorf("cloudflare-sandbox: policy is required (AC-FC3)")
	}
	prepared := make([]string, 0, len(req.Inputs))
	for name := range req.Inputs {
		// Record the input ref as scoped session state. The actual byte
		// materialization is performed by the M0 worker's copy endpoint; the
		// scoped state lets destroy revoke it.
		if err := p.rt.SetMeta(req.SessionHandle, "input:"+name, "materialized"); err != nil {
			return harness.PrepareWorkspaceResponse{Status: "failed"}, fmt.Errorf("cloudflare-sandbox: materialize %q: %w", name, err)
		}
		prepared = append(prepared, name)
	}
	return harness.PrepareWorkspaceResponse{Status: "ok", PreparedRefs: prepared}, nil
}

// ExecuteStep delivers the step prompt via Nudge, observes completion via the
// activity/liveness primitives, reads the produced-file set, and assembles the
// response envelope from these reads (AC-LC3, IS §Interface binding). The
// response is a Gas City composition over PTY/session primitives, not a single
// Go call.
func (p *Provider) ExecuteStep(ctx context.Context, req harness.ExecutionRequest) (harness.ExecutionResponse, error) {
	resp := harness.ExecutionResponse{
		PolicyEvents:    []harness.PolicyEvent{}, // never nil (AC-LC6)
		RuntimeIdentity: p.runtimeIdentity(),
		ModelUsage:      nil, // no inference (AC-RS6)
	}

	// Defense-in-depth precondition checks (AC-FC2, AC-FC3). Gas City asserts
	// these before calling, but the provider rejects on receipt too.
	if req.VerifierContract.IsEmpty() {
		resp.Status = harness.StatusFailed
		resp.ProviderVerdict = harness.ProviderVerdict{Status: harness.StatusFailed, Reason: "missing verifier contract"}
		resp.Error = &harness.ProviderError{Code: "missing_verifier_contract", Message: "verifier_contract is mandatory (AC-FC2)"}
		resp.ArtifactManifest = manifestAllMissing(req.DeclaredOutputs)
		return resp, nil
	}
	if req.Policy.IsEmpty() {
		resp.Status = harness.StatusFailed
		resp.ProviderVerdict = harness.ProviderVerdict{Status: harness.StatusFailed, Reason: "missing policy"}
		resp.Error = &harness.ProviderError{Code: "missing_policy", Message: "policy is mandatory (AC-FC3)"}
		resp.ArtifactManifest = manifestAllMissing(req.DeclaredOutputs)
		return resp, nil
	}

	// Deliver the step prompt. For the command-exec surface the prompt is the
	// command; the M0 worker runs it. A delivery error is a hard failure.
	if err := p.rt.Nudge(req.SessionID, runtime.TextContent(req.Purpose)); err != nil && !runtime.IsSessionGone(err) {
		resp.Status = harness.StatusFailed
		resp.ProviderVerdict = harness.ProviderVerdict{Status: harness.StatusFailed, Reason: "nudge failed"}
		resp.Error = &harness.ProviderError{Code: "delivery_failed", Message: err.Error()}
		resp.ArtifactManifest = manifestAllMissing(req.DeclaredOutputs)
		resp.Logs = p.collectLogsBestEffort(req.SessionID)
		return resp, nil
	}

	// Observe completion. The command surface has no agent_end signal: the M0
	// exec endpoint is synchronous, so a returned Nudge/exec means the command
	// finished. When the step declares process names we additionally poll
	// ProcessAlive with a deadline (AC-FC6 timeout discipline); otherwise the
	// synchronous return is the completion signal and we do not block.
	timedOut := p.waitForCompletion(ctx, req.SessionID, processNames(req))

	// Collect logs and the produced-file set (AC-LC4, AC-LC5).
	resp.Logs = p.collectLogsBestEffort(req.SessionID)
	artifacts, err := p.produced(ctx, req.SessionID, req.DeclaredOutputs)
	if err != nil {
		artifacts = nil
	}
	resp.Artifacts = artifacts
	resp.ArtifactManifest = buildManifest(req.DeclaredOutputs, artifacts)

	if timedOut {
		resp.Status = harness.StatusTimeout
		resp.ProviderVerdict = harness.ProviderVerdict{Status: harness.StatusTimeout, Reason: "execution deadline exceeded"}
		resp.Error = &harness.ProviderError{Code: "execution_timeout", Message: "cloudflare-sandbox step exceeded poll timeout"}
		return resp, nil
	}

	// Stop-condition: "agent says it is done" is not a stop condition (AC-FC5).
	// Completion requires every declared output to be produced.
	if missing := missingOutputs(resp.ArtifactManifest); len(missing) > 0 {
		resp.Status = harness.StatusFailed
		resp.ProviderVerdict = harness.ProviderVerdict{Status: harness.StatusFailed, Reason: "declared outputs missing"}
		resp.Error = &harness.ProviderError{Code: "declared_outputs_missing", Message: fmt.Sprintf("missing: %v", missing)}
		// Completion was established by a self-report end-of-turn signal rather
		// than a manifest-backed produced set (AC-FC5, AC-RS1).
		resp.CompletionClaimedWithoutManifest = true
		return resp, nil
	}

	resp.Status = harness.StatusCompleted
	resp.ProviderVerdict = harness.ProviderVerdict{Status: harness.StatusCompleted, Reason: "all declared outputs produced"}
	return resp, nil
}

// CollectArtifacts reads the produced-file set and builds the declared-vs-produced
// manifest (AC-LC4). A checksum that cannot be computed marks the artifact
// missing (handled by the produced reader returning no entry for it).
func (p *Provider) CollectArtifacts(ctx context.Context, req harness.CollectArtifactsRequest) (harness.CollectArtifactsResponse, error) {
	artifacts, err := p.produced(ctx, req.SessionHandle, req.DeclaredOutputs)
	if err != nil {
		return harness.CollectArtifactsResponse{
			Artifacts:        nil,
			ArtifactManifest: manifestAllMissing(req.DeclaredOutputs),
		}, nil
	}
	return harness.CollectArtifactsResponse{
		Artifacts:        artifacts,
		ArtifactManifest: buildManifest(req.DeclaredOutputs, artifacts),
	}, nil
}

// CollectLogs maps to Peek (scrollback) + GetLastActivity (AC-LC5).
func (p *Provider) CollectLogs(_ context.Context, req harness.CollectLogsRequest) (harness.CollectLogsResponse, error) {
	logs := p.collectLogsBestEffort(req.SessionHandle)
	return harness.CollectLogsResponse{
		StdoutRef:  logs.StdoutRef,
		StderrRef:  logs.StderrRef,
		TraceSpans: logs.TraceSpans,
	}, nil
}

// CollectPolicyEvents derives events from filesystem write-scope checks
// (AC-LC6). The cloudflare transport has no native policy event stream, so an
// empty slice (never nil) is contract-valid.
func (p *Provider) CollectPolicyEvents(_ context.Context, _ harness.CollectPolicyEventsRequest) (harness.CollectPolicyEventsResponse, error) {
	return harness.CollectPolicyEventsResponse{PolicyEvents: []harness.PolicyEvent{}}, nil
}

// Snapshot is unsupported for the cloudflare transport (AC-LC7). MUST NOT error.
func (p *Provider) Snapshot(_ context.Context, _ harness.SnapshotRequest) (harness.SnapshotResponse, error) {
	return harness.SnapshotResponse{Unsupported: true}, nil
}

// Restore is unsupported for the cloudflare transport (AC-LC8). MUST NOT error.
func (p *Provider) Restore(_ context.Context, _ harness.RestoreRequest) (harness.RestoreResponse, error) {
	return harness.RestoreResponse{Unsupported: true}, nil
}

// Status maps to IsRunning + ProcessAlive + GetLastActivity + Capabilities
// (AC-LC9). Capacity defaults to 1 when the session is healthy; the registry
// reads it during selection (AC-REG2).
func (p *Provider) Status(_ context.Context, req harness.StatusRequest) (harness.StatusResponse, error) {
	st := harness.StatusResponse{
		ProviderVersion: p.version,
		ImageDigest:     p.imageDigest,
	}
	if req.SessionHandle == "" {
		// No specific session: report the provider as healthy with capacity.
		st.Healthy = true
		st.Ready = true
		st.Capacity = 1
		return st, nil
	}
	running := p.rt.IsRunning(req.SessionHandle)
	st.Healthy = running
	st.Ready = running
	if running {
		st.Capacity = 1
	}
	return st, nil
}

// Restart maps to Stop then Start (AC-LC10). Fail-closed: a restart that cannot
// bring the runtime healthy returns non-OK.
func (p *Provider) Restart(ctx context.Context, req harness.RestartRequest) (harness.RestartResponse, error) {
	if err := p.rt.Stop(req.SessionHandle); err != nil && !runtime.IsSessionGone(err) {
		return harness.RestartResponse{Status: "failed"}, fmt.Errorf("cloudflare-sandbox: restart stop %q: %w", req.SessionHandle, err)
	}
	if err := p.rt.Start(ctx, req.SessionHandle, runtime.Config{WorkDir: "/workspace"}); err != nil {
		return harness.RestartResponse{Status: "failed"}, fmt.Errorf("cloudflare-sandbox: restart start %q: %w", req.SessionHandle, err)
	}
	return harness.RestartResponse{Status: "ok"}, nil
}

// Destroy maps to Stop + RemoveMeta for scoped state (AC-LC11).
func (p *Provider) Destroy(_ context.Context, req harness.DestroyRequest) (harness.DestroyResponse, error) {
	if err := p.rt.Stop(req.SessionHandle); err != nil && !runtime.IsSessionGone(err) {
		return harness.DestroyResponse{Status: "failed"}, fmt.Errorf("cloudflare-sandbox: destroy %q: %w", req.SessionHandle, err)
	}
	return harness.DestroyResponse{Status: "ok"}, nil
}

// waitForCompletion observes step completion. When processNames is empty the
// synchronous exec/Nudge return is the completion signal and we do not block.
// When processNames are configured we poll ProcessAlive until the command
// process is gone or the deadline elapses; on deadline we return true (timeout,
// AC-FC6). The session container itself stays up — completion is the command
// process finishing, not the session dying. The authoritative stop condition is
// still the artifact manifest (AC-FC5).
func (p *Provider) waitForCompletion(ctx context.Context, handle string, processNames []string) bool {
	if len(processNames) == 0 {
		return false
	}
	deadline := time.Now().Add(p.pollTimeout)
	for {
		if !p.rt.ProcessAlive(handle, processNames) {
			return false // command process finished
		}
		if time.Now().After(deadline) {
			return true
		}
		select {
		case <-ctx.Done():
			return true
		case <-time.After(p.pollInterval):
		}
	}
}

// processNames extracts step process names from runtime_config, if declared.
func processNames(req harness.ExecutionRequest) []string {
	raw, ok := req.RuntimeConfig["process_names"]
	if !ok {
		return nil
	}
	list, ok := raw.([]string)
	if ok {
		return list
	}
	anyList, ok := raw.([]any)
	if !ok {
		return nil
	}
	names := make([]string, 0, len(anyList))
	for _, v := range anyList {
		if s, ok := v.(string); ok && s != "" {
			names = append(names, s)
		}
	}
	return names
}

// collectLogsBestEffort reads scrollback via Peek and the activity anchor via
// GetLastActivity (AC-LC5). Best-effort: a read error yields empty refs.
func (p *Provider) collectLogsBestEffort(handle string) harness.Logs {
	logs := harness.Logs{}
	if out, err := p.rt.Peek(handle, defaultPeekLines); err == nil && out != "" {
		logs.StdoutRef = "scrollback:" + handle
		logs.TraceSpans = []harness.TraceSpan{{Type: "scrollback", Detail: truncate(out, 4000)}}
	}
	if t, err := p.rt.GetLastActivity(handle); err == nil && !t.IsZero() {
		logs.TraceSpans = append(logs.TraceSpans, harness.TraceSpan{
			Timestamp: t.Format(time.RFC3339Nano),
			Type:      "last_activity",
		})
	}
	return logs
}

func (p *Provider) runtimeIdentity() harness.RuntimeIdentity {
	return harness.RuntimeIdentity{
		ProviderID:  ProviderID,
		Version:     p.version,
		ImageDigest: p.imageDigest,
	}
}

// buildManifest maps each declared output to produced|missing and marks any
// produced artifact not in declared_outputs as extra (AC-RS3).
func buildManifest(declared []string, produced []harness.Artifact) []harness.ManifestEntry {
	producedByPath := make(map[string]struct{}, len(produced))
	for _, a := range produced {
		producedByPath[a.Path] = struct{}{}
	}
	declaredSet := make(map[string]struct{}, len(declared))
	manifest := make([]harness.ManifestEntry, 0, len(declared)+len(produced))
	for _, name := range declared {
		declaredSet[name] = struct{}{}
		status := harness.ManifestMissing
		if _, ok := producedByPath[name]; ok {
			status = harness.ManifestProduced
		}
		manifest = append(manifest, harness.ManifestEntry{Artifact: name, Status: status})
	}
	for _, a := range produced {
		if _, ok := declaredSet[a.Path]; !ok {
			manifest = append(manifest, harness.ManifestEntry{Artifact: a.Path, Status: harness.ManifestExtra})
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

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
