package telemetry

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// TelemetryEvent is the queue payload schema consumed by ff-pipeline telemetry.
type TelemetryEvent struct {
	TraceID      string                 `json:"trace_id"`
	SpanID       string                 `json:"span_id"`
	ParentSpanID string                 `json:"parent_span_id,omitempty"`
	Name         string                 `json:"name"`
	Service      string                 `json:"service"`
	StartTimeMS  int64                  `json:"start_time_ms"`
	DurationMS   int64                  `json:"duration_ms"`
	Outcome      string                 `json:"outcome"`
	Error        string                 `json:"error,omitempty"`
	Attrs        map[string]interface{} `json:"attrs"`
}

// Emitter batches telemetry and posts to supervisor /internal/telemetry.
type Emitter struct {
	supervisorURL string
	token         string
	client        *http.Client

	mu    sync.Mutex
	batch []TelemetryEvent
}

// NewQueueEmitterFromEnv returns a best-effort emitter configured from env.
// GC_TELEMETRY_URL wins. Otherwise we derive from GC_BEAD_STORE_URL.
func NewQueueEmitterFromEnv() *Emitter {
	supervisorURL := strings.TrimSpace(os.Getenv("GC_TELEMETRY_URL"))
	if supervisorURL == "" {
		beadURL := strings.TrimSpace(os.Getenv("GC_BEAD_STORE_URL"))
		if i := strings.Index(beadURL, "/internal/bead-store/"); i > 0 {
			supervisorURL = beadURL[:i]
		}
	}
	return &Emitter{
		supervisorURL: strings.TrimRight(supervisorURL, "/"),
		token:         strings.TrimSpace(os.Getenv("GC_SUPERVISOR_TOKEN")),
		client:        &http.Client{Timeout: 3 * time.Second},
	}
}

func (e *Emitter) Enabled() bool {
	return e != nil && e.supervisorURL != "" && e.token != ""
}

func (e *Emitter) Emit(event TelemetryEvent) {
	if e == nil || !e.Enabled() {
		return
	}
	e.mu.Lock()
	e.batch = append(e.batch, event)
	e.mu.Unlock()
}

func (e *Emitter) Flush() error {
	if e == nil || !e.Enabled() {
		return nil
	}
	e.mu.Lock()
	events := append([]TelemetryEvent(nil), e.batch...)
	e.batch = nil
	e.mu.Unlock()
	if len(events) == 0 {
		return nil
	}

	body, err := json.Marshal(events)
	if err != nil {
		fmt.Fprintf(os.Stderr, "telemetry flush marshal: %v\n", err) //nolint:errcheck
		return err
	}
	req, err := http.NewRequest(http.MethodPost, e.supervisorURL+"/internal/telemetry", bytes.NewReader(body))
	if err != nil {
		fmt.Fprintf(os.Stderr, "telemetry flush request: %v\n", err) //nolint:errcheck
		return err
	}
	req.Header.Set("Authorization", "Bearer "+e.token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := e.client.Do(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "telemetry flush send: %v\n", err) //nolint:errcheck
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		err = fmt.Errorf("telemetry flush status=%d", resp.StatusCode)
		fmt.Fprintf(os.Stderr, "%v\n", err) //nolint:errcheck
		return err
	}
	return nil
}
