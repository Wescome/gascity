package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/harness"
)

// fidelityReleaseScriptPath is the in-container path to the deployed fidelity
// release driver. It is a package var (not a const) so tests can point it at a
// fake script; production always runs the deployed script.
var fidelityReleaseScriptPath = "/data/cities/factory/fidelity/fidelity-release.sh"

// defaultFidelityMaxAmendmentDepth is the convergence ceiling used when a bead
// carries no gc.max_iterations metadata.
const defaultFidelityMaxAmendmentDepth = 5
const defaultFidelityFactoryAttempt = 1

// defaultFidelityWebhookKeyID is the HMAC key id used when a bead carries no
// gc.ff_webhook_hmac_keyid metadata.
const defaultFidelityWebhookKeyID = "v1"
const defaultFidelityWebhookURL = "https://ff-pipeline.koales.workers.dev/webhooks/gascity"

// Fidelity validator verdicts, surfaced to the caller so it can re-enter the
// molecule (revise) or stop (fail-closed). The bead itself is closed/POSTed by
// the fidelity-release driver, not by Gas City — these errors are the signal,
// not the action.
var (
	// ErrFidelityRevise reports the fidelity validator returned exit 10: the
	// molecule must re-enter at the Code step.
	ErrFidelityRevise = errors.New("fidelity validation: revise")
	// ErrFidelityFailClosed reports the fidelity validator returned exit 20 (or
	// any other non-zero, non-revise exit): the molecule.failed event has been
	// POSTed and the step fails closed.
	ErrFidelityFailClosed = errors.New("fidelity validation: fail-closed")
)

// fidelity-release.sh exit codes (IS-GC-FIDELITY-VALIDATION release step).
const (
	fidelityExitRelease    = 0  // RELEASE POSTed + accepted; bead closed by driver.
	fidelityExitRevise     = 10 // Molecule re-enters at the Code step.
	fidelityExitFailClosed = 20 // molecule.failed POSTed; fail closed.
)

// fidelityLineage is the lineage block of the fidelity job. factory_attempt is
// a bare JSON integer (not a string) per the release step contract.
type fidelityLineage struct {
	FnID           string `json:"fn_id"`
	IsID           string `json:"is_id"`
	EsID           string `json:"es_id"`
	EpID           string `json:"ep_id"`
	FormID         string `json:"form_id"`
	FactoryAttempt int    `json:"factory_attempt"`
	BeadID         string `json:"bead_id"`
}

// fidelityResponse mirrors the provider ExecutionResponse fields the release
// step reads. PolicyEvents is a non-nil slice so it serializes as [] not null.
type fidelityResponse struct {
	Status                           harness.ProviderStatus  `json:"status"`
	ProviderVerdict                  harness.ProviderVerdict `json:"provider_verdict"`
	Artifacts                        []harness.Artifact      `json:"artifacts"`
	ArtifactManifest                 []fidelityManifestEntry `json:"artifact_manifest"`
	PolicyEvents                     []harness.PolicyEvent   `json:"policy_events"`
	ModelUsage                       *harness.ModelUsage     `json:"model_usage"`
	RuntimeIdentity                  harness.RuntimeIdentity `json:"runtime_identity"`
	SessionArchiveRef                string                  `json:"session_archive_ref"`
	VerifierReportRef                string                  `json:"verifier_report_ref"`
	Error                            *harness.ProviderError  `json:"error"`
	CompletionClaimedWithoutManifest bool                    `json:"completion_claimed_without_manifest"`
	StepOutputs                      map[string]any          `json:"step_outputs"`
}

type fidelityManifestEntry struct {
	Name     string                 `json:"name"`
	State    harness.ManifestStatus `json:"state"`
	Checksum string                 `json:"checksum,omitempty"`
}

// fidelityConvergence carries the amendment ceiling.
type fidelityConvergence struct {
	MaxAmendmentDepth int `json:"max_amendment_depth"`
}

// fidelityWebhook carries the FF webhook wiring. hmac_secret is sourced from
// the environment ($GAS_CITY_HMAC_SECRET) by the script, not embedded here.
type fidelityWebhook struct {
	URL        string `json:"url"`
	HMACSecret string `json:"hmac_secret"`
	KeyID      string `json:"key_id"`
}

// fidelityJob is the complete $RIG_ROOT/fidelity-job.json document.
type fidelityJob struct {
	StepName          string              `json:"step_name"`
	IsReleaseStep     bool                `json:"is_release_step"`
	Lineage           fidelityLineage     `json:"lineage"`
	DeclaredOutputs   []string            `json:"declared_outputs"`
	PriorStepVerdicts []any               `json:"prior_step_verdicts"`
	Response          fidelityResponse    `json:"response"`
	Convergence       fidelityConvergence `json:"convergence"`
	Webhook           fidelityWebhook     `json:"webhook"`
}

// runFidelityValidator feeds the provider ExecutionResponse into the deployed
// fidelity release driver. It writes $RIG_ROOT/fidelity-job.json, runs
// fidelity-release.sh under the dispatch context, and maps the script's exit
// code onto the molecule lifecycle:
//
//	0  -> RELEASE POSTed + accepted; the driver closed the bead. Returns nil.
//	10 -> Revise; stamps gc.harness_fidelity_verdict=revise, returns ErrFidelityRevise.
//	20 -> Fail-closed; stamps gc.harness_fidelity_verdict=fail_closed, returns ErrFidelityFailClosed.
//	*  -> Any other non-zero exit is treated as fail-closed.
//
// The fidelity verdict is the molecule verdict, distinct from the provider
// status (AC-RS2): the provider says "I ran", fidelity says "release / revise /
// fail".
func runFidelityValidator(ctx context.Context, store beads.Store, bead beads.Bead, cfg *config.City, cityPath string, resp harness.ExecutionResponse, stderr io.Writer) error {
	rigRoot := fidelityRigRoot(bead, cityPath)

	job := buildFidelityJob(store, bead, resp)
	if err := writeFidelityJob(rigRoot, job); err != nil {
		return fmt.Errorf("fidelity job bead=%s: %w", bead.ID, err)
	}

	// WP-OBS-4: fidelity.run fires as the release step hands the accumulated
	// molecule evidence to the validator. prior_step_count is the number of
	// executed prior-step verdicts the validator will weigh.
	emitFidelityRun(store, bead, len(job.PriorStepVerdicts))
	fidelityStart := time.Now()

	cmd := exec.CommandContext(ctx, "bash", fidelityReleaseScriptPath)
	cmd.Env = append(os.Environ(),
		"GC_BEAD_ID="+bead.ID,
		"FN_ID="+bead.Metadata["gc.fn_id"],
		"RIG_ROOT="+rigRoot,
		"FF_WEBHOOK_URL="+bead.Metadata["gc.ff_webhook_url"],
		"FF_WEBHOOK_HMAC_KEYID="+webhookHmacKeyid(bead),
		// GAS_CITY_HMAC_SECRET is inherited from os.Environ() so the secret is
		// never read or echoed by Gas City.
	)
	cmd.Stdout = stderr
	cmd.Stderr = stderr

	runErr := cmd.Run()
	exitCode := fidelityExitCode(runErr)

	switch exitCode {
	case fidelityExitRelease:
		// RELEASE was POSTed and accepted; the driver already closed the bead.
		emitFidelityVerdict(store, bead, "release", fidelityStart)
		// WP-OBS-4: a clean release closes the molecule root bead. This is the
		// molecule's terminal outcome and the per-molecule flush point.
		emitMoleculeComplete(store, bead, "approved", fidelityStart)
		_, _ = fmt.Fprintf(stderr, "fidelity validation: bead=%s verdict=release\n", bead.ID)
		return nil
	case fidelityExitRevise:
		recordFidelityVerdict(store, bead.ID, "revise", stderr)
		// revise is NOT a molecule-terminal outcome: the molecule re-enters at
		// the Code step, so no molecule.complete is emitted here.
		emitFidelityVerdict(store, bead, "revise", fidelityStart)
		_, _ = fmt.Fprintf(stderr, "fidelity validation: bead=%s verdict=revise\n", bead.ID)
		return ErrFidelityRevise
	case fidelityExitFailClosed:
		recordFidelityVerdict(store, bead.ID, "fail_closed", stderr)
		failHarnessStepClosed(store, bead.ID, "fidelity_fail_closed", ErrFidelityFailClosed.Error(), stderr)
		emitFidelityVerdict(store, bead, "fail_closed", fidelityStart)
		emitMoleculeComplete(store, bead, "failed", fidelityStart)
		_, _ = fmt.Fprintf(stderr, "fidelity validation: bead=%s verdict=fail_closed\n", bead.ID)
		return ErrFidelityFailClosed
	default:
		// Any other non-zero exit is treated as fail-closed: the validator did
		// not give us a clean release or a structured revise, so we must not
		// proceed as if the step succeeded.
		recordFidelityVerdict(store, bead.ID, "fail_closed", stderr)
		failHarnessStepClosed(store, bead.ID, "fidelity_fail_closed", fmt.Sprintf("fidelity-release.sh exit=%d", exitCode), stderr)
		emitFidelityVerdict(store, bead, "fail_closed", fidelityStart)
		emitMoleculeComplete(store, bead, "failed", fidelityStart)
		_, _ = fmt.Fprintf(stderr, "fidelity validation: bead=%s verdict=fail_closed (unexpected exit=%d)\n", bead.ID, exitCode)
		return fmt.Errorf("%w: fidelity-release.sh exit=%d", ErrFidelityFailClosed, exitCode)
	}
}

// fidelityRigRoot resolves the rig root from bead metadata, falling back to
// cityPath/rigs/<formula_id> when gc.rig_root is absent.
func fidelityRigRoot(bead beads.Bead, cityPath string) string {
	if root := bead.Metadata["gc.rig_root"]; root != "" {
		return root
	}
	return filepath.Join(cityPath, "rigs", bead.Metadata["gc.formula_id"])
}

// buildFidelityJob assembles the fidelity job document from bead lineage
// metadata and the provider response. DeclaredOutputs is read from the Release
// bead; PriorStepVerdicts is accumulated from the serialized responses stamped
// on sibling beads (E2 envelope accumulation). Without these the fidelity
// validator fails closed on empty prior verdicts regardless of routing.
func buildFidelityJob(store beads.Store, bead beads.Bead, resp harness.ExecutionResponse) fidelityJob {
	lineage := fidelityLineageFromBead(bead)
	return fidelityJob{
		StepName:      "release",
		IsReleaseStep: true,
		Lineage: fidelityLineage{
			FnID:           lineage.FnID,
			IsID:           lineage.IsID,
			EsID:           lineage.EsID,
			EpID:           lineage.EpID,
			FormID:         lineage.FormID,
			FactoryAttempt: lineage.FactoryAttempt,
			BeadID:         bead.ID,
		},
		DeclaredOutputs:   harnessDeclaredOutputsForBead(bead),
		PriorStepVerdicts: fidelityPriorStepVerdicts(store, bead, os.Stderr),
		Response:          fidelityResponseFrom(resp),
		Convergence: fidelityConvergence{
			MaxAmendmentDepth: fidelityMaxAmendmentDepth(bead),
		},
		Webhook: fidelityWebhook{
			URL:        fidelityWebhookURL(bead),
			HMACSecret: os.Getenv("GAS_CITY_HMAC_SECRET"),
			KeyID:      webhookHmacKeyid(bead),
		},
	}
}

// fidelityPriorStepVerdicts accumulates the serialized provider responses
// stamped on sibling beads (gc.harness_response_json) into the prior-step
// verdict envelope the fidelity validator consumes. Sibling beads are found by
// shared gc.root_bead_id; the Release step itself is excluded (it has no prior
// verdict of its own). On a missing root id or store error the slice is empty
// (non-fatal): prior verdicts become partial, not a hard failure.
func fidelityPriorStepVerdicts(store beads.Store, bead beads.Bead, stderr io.Writer) []any {
	verdicts := []any{}
	rootBeadID := strings.TrimSpace(bead.Metadata["gc.root_bead_id"])
	if rootBeadID == "" {
		fidelityLogf(stderr, "fidelity validation: bead=%s no gc.root_bead_id; prior_step_verdicts empty\n", bead.ID)
		return verdicts
	}
	siblings, err := store.ListByMetadata(map[string]string{"gc.root_bead_id": rootBeadID}, 0, beads.IncludeClosed)
	if err != nil {
		fidelityLogf(stderr, "fidelity validation: bead=%s sibling lookup error: %v; prior_step_verdicts empty\n", bead.ID, err)
		return verdicts
	}

	// Keep only siblings that actually executed (have a stamped response) and
	// are not the Release step itself, then order them earliest-first so
	// step_index is stable: 0 = earliest executed prior step.
	eligible := make([]beads.Bead, 0, len(siblings))
	for _, sibling := range siblings {
		if strings.TrimSpace(sibling.Metadata["gc.harness_response_json"]) == "" {
			continue
		}
		if isHarnessReleaseStep(sibling) {
			continue
		}
		eligible = append(eligible, sibling)
	}
	sort.SliceStable(eligible, func(a, b int) bool {
		return eligible[a].CreatedAt.Before(eligible[b].CreatedAt)
	})

	for i, sibling := range eligible {
		// Build a PriorStepVerdict-shaped object from each sibling bead.
		// The validator expects {step_index, step_name, outcome, remediation}.
		// gc.outcome=="pass" → outcome="approved"; anything else → "revise".
		outcome := "revise"
		if strings.TrimSpace(sibling.Metadata["gc.outcome"]) == "pass" {
			outcome = "approved"
		}
		stepName := strings.TrimSpace(sibling.Metadata["gc.step_ref"])
		if stepName == "" {
			stepName = sibling.Ref
		}
		verdict := map[string]any{
			"step_index":  i, // i is the loop index (0-based, earliest first)
			"step_name":   stepName,
			"outcome":     outcome,
			"remediation": "",
		}
		verdicts = append(verdicts, verdict)
	}
	return verdicts
}

// fidelityLogf writes to stderr when non-nil, tolerating a nil writer so
// callers that lack a stderr handle (buildFidelityJob) stay non-fatal.
func fidelityLogf(stderr io.Writer, format string, args ...any) {
	if stderr == nil {
		return
	}
	_, _ = fmt.Fprintf(stderr, format, args...)
}

func fidelityLineageFromBead(bead beads.Bead) fidelityLineage {
	lineage := fidelityLineage{
		FnID:           strings.TrimSpace(bead.Metadata["gc.fn_id"]),
		IsID:           strings.TrimSpace(bead.Metadata["gc.is_id"]),
		EsID:           strings.TrimSpace(bead.Metadata["gc.es_id"]),
		EpID:           strings.TrimSpace(bead.Metadata["gc.ep_id"]),
		FormID:         strings.TrimSpace(bead.Metadata["gc.form_id"]),
		FactoryAttempt: fidelityFactoryAttempt(bead),
		BeadID:         bead.ID,
	}
	for key, value := range parseFidelityLineageDescription(bead.Description) {
		switch key {
		case "fn":
			if lineage.FnID == "" {
				lineage.FnID = value
			}
		case "is":
			if lineage.IsID == "" {
				lineage.IsID = value
			}
		case "es":
			if lineage.EsID == "" {
				lineage.EsID = value
			}
		case "ep":
			if lineage.EpID == "" {
				lineage.EpID = value
			}
		case "form":
			if lineage.FormID == "" {
				lineage.FormID = value
			}
		case "attempt":
			if lineage.FactoryAttempt == defaultFidelityFactoryAttempt {
				if n, err := strconv.Atoi(value); err == nil && n > 0 {
					lineage.FactoryAttempt = n
				}
			}
		}
	}
	return lineage
}

var fidelityLineagePattern = regexp.MustCompile(`\b(fn|is|es|ep|form|attempt)=([^\s]+)`)

func parseFidelityLineageDescription(description string) map[string]string {
	matches := fidelityLineagePattern.FindAllStringSubmatch(description, -1)
	out := make(map[string]string, len(matches))
	for _, match := range matches {
		if len(match) == 3 {
			out[match[1]] = strings.TrimSpace(match[2])
		}
	}
	return out
}

// fidelityResponseFrom copies the provider response into the job-embedded
// shape, normalizing PolicyEvents to a non-nil slice so it serializes as []
// rather than null when empty (release step contract).
func fidelityResponseFrom(resp harness.ExecutionResponse) fidelityResponse {
	events := resp.PolicyEvents
	if events == nil {
		events = []harness.PolicyEvent{}
	}
	return fidelityResponse{
		Status:                           resp.Status,
		ProviderVerdict:                  resp.ProviderVerdict,
		Artifacts:                        resp.Artifacts,
		ArtifactManifest:                 fidelityManifestFrom(resp),
		PolicyEvents:                     events,
		ModelUsage:                       resp.ModelUsage,
		RuntimeIdentity:                  resp.RuntimeIdentity,
		SessionArchiveRef:                resp.SessionArchiveRef,
		VerifierReportRef:                resp.VerifierReportRef,
		Error:                            resp.Error,
		CompletionClaimedWithoutManifest: resp.CompletionClaimedWithoutManifest,
		StepOutputs:                      resp.StepOutputs,
	}
}

func fidelityManifestFrom(resp harness.ExecutionResponse) []fidelityManifestEntry {
	checksums := make(map[string]string, len(resp.Artifacts))
	for _, artifact := range resp.Artifacts {
		if artifact.Path != "" && artifact.Checksum != "" {
			checksums[artifact.Path] = artifact.Checksum
		}
	}
	out := make([]fidelityManifestEntry, 0, len(resp.ArtifactManifest))
	for _, entry := range resp.ArtifactManifest {
		converted := fidelityManifestEntry{
			Name:  entry.Artifact,
			State: entry.Status,
		}
		if entry.Status == harness.ManifestProduced {
			converted.Checksum = checksums[entry.Artifact]
		}
		out = append(out, converted)
	}
	return out
}

// writeFidelityJob serializes the job to rigRoot/fidelity-job.json, creating
// the rig root directory if needed.
func writeFidelityJob(rigRoot string, job fidelityJob) error {
	if err := os.MkdirAll(rigRoot, 0o755); err != nil {
		return fmt.Errorf("creating rig root %s: %w", rigRoot, err)
	}
	data, err := json.MarshalIndent(job, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling fidelity job: %w", err)
	}
	jobPath := filepath.Join(rigRoot, "fidelity-job.json")
	if err := os.WriteFile(jobPath, data, 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", jobPath, err)
	}
	return nil
}

// fidelityFactoryAttempt parses gc.factory_attempt as an integer, defaulting to
// 1 on absence or parse error (the contract requires a bare integer).
func fidelityFactoryAttempt(bead beads.Bead) int {
	if v, err := strconv.Atoi(bead.Metadata["gc.factory_attempt"]); err == nil {
		return v
	}
	return defaultFidelityFactoryAttempt
}

// fidelityMaxAmendmentDepth parses gc.max_iterations, defaulting to 5.
func fidelityMaxAmendmentDepth(bead beads.Bead) int {
	if v, err := strconv.Atoi(bead.Metadata["gc.max_iterations"]); err == nil && v > 0 {
		return v
	}
	return defaultFidelityMaxAmendmentDepth
}

// webhookHmacKeyid reads gc.ff_webhook_hmac_keyid, defaulting to v1.
func webhookHmacKeyid(bead beads.Bead) string {
	if k := bead.Metadata["gc.ff_webhook_hmac_keyid"]; k != "" {
		return k
	}
	return defaultFidelityWebhookKeyID
}

func fidelityWebhookURL(bead beads.Bead) string {
	if url := strings.TrimSpace(bead.Metadata["gc.ff_webhook_url"]); url != "" {
		return url
	}
	return defaultFidelityWebhookURL
}

// fidelityExitCode extracts the process exit code from a *exec.ExitError. A nil
// error is exit 0; a non-ExitError (e.g. the script could not start) is mapped
// to a non-zero sentinel so it is treated as fail-closed.
func fidelityExitCode(err error) int {
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	// Could not even start the script (missing binary, context cancelled, etc).
	// Treat as a non-zero, non-revise exit so it fails closed.
	return -1
}

// recordFidelityVerdict stamps the molecule verdict onto the bead for
// replay-identity. Best-effort: a metadata write failure is logged but does not
// change the returned verdict.
func recordFidelityVerdict(store beads.Store, beadID, verdict string, stderr io.Writer) {
	if err := store.Update(beadID, beads.UpdateOpts{
		Metadata: map[string]string{
			"gc.harness_fidelity_verdict":    verdict,
			"gc.harness_fidelity_verdict_at": time.Now().UTC().Format(time.RFC3339),
		},
	}); err != nil {
		_, _ = fmt.Fprintf(stderr, "fidelity validation: bead=%s verdict-stamp error: %v\n", beadID, err)
	}
}
