package cloudflare

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// pollAlive polls p.IsRunning until the session is alive or deadline expires.
func pollAlive(t *testing.T, p *Provider, name string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if p.IsRunning(name) {
			return
		}
		time.Sleep(3 * time.Second)
	}
	t.Fatalf("session %q not alive after %s", name, timeout)
}

// TestLiveProviderSuite runs a full E2E suite against the real deployed
// Cloudflare Worker + Sandbox. Requires GC_CLOUDFLARE_RUNTIME_URL (and
// optionally GC_CLOUDFLARE_RUNTIME_TOKEN) to be set.
//
// Run with:
//
//	GC_CLOUDFLARE_RUNTIME_URL=https://... \
//	  go test -v -run TestLiveProviderSuite -timeout 10m ./internal/runtime/cloudflare/
func TestLiveProviderSuite(t *testing.T) {
	endpoint := os.Getenv("GC_CLOUDFLARE_RUNTIME_URL")
	if endpoint == "" {
		t.Skip("GC_CLOUDFLARE_RUNTIME_URL not set — skipping live integration test")
	}

	p, err := NewProvider()
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}

	beforeStart := time.Now()

	// Boot one shared session for the bulk of the suite. Cold boot = 30-90s;
	// sharing avoids paying that cost per test.
	name := fmt.Sprintf("live-suite-%d", beforeStart.UnixNano())
	t.Logf("shared session: %s", name)

	if err := p.Start(context.Background(), name, runtime.Config{WorkDir: "/workspace"}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = p.Stop(name) })

	t.Log("polling for session alive (up to 120s)...")
	pollAlive(t, p, name, 120*time.Second)
	t.Log("session alive — running sub-tests")

	// ── Liveness ─────────────────────────────────────────────────────────────

	t.Run("IsRunning_TrueForAliveSession", func(t *testing.T) {
		if !p.IsRunning(name) {
			t.Fatal("IsRunning = false, want true")
		}
	})

	t.Run("IsRunning_FalseForUnknownSession", func(t *testing.T) {
		if p.IsRunning("live-suite-definitely-not-started-xyz") {
			t.Fatal("IsRunning = true for unknown session, want false")
		}
	})

	// ── Activity timestamp ────────────────────────────────────────────────────

	t.Run("GetLastActivity_ReturnsCreatedAt", func(t *testing.T) {
		got, err := p.GetLastActivity(name)
		if err != nil {
			t.Fatalf("GetLastActivity: %v", err)
		}
		if got.IsZero() {
			t.Fatal("GetLastActivity = zero time, want non-zero createdAt from .m0-session.json")
		}
		if got.Before(beforeStart.Add(-5 * time.Minute)) {
			t.Fatalf("GetLastActivity = %s, suspiciously old (before %s)", got, beforeStart)
		}
		t.Logf("createdAt: %s", got)
	})

	// ── Static methods (no network round-trip) ────────────────────────────────

	t.Run("IsAttached_AlwaysFalse", func(t *testing.T) {
		if p.IsAttached(name) {
			t.Fatal("IsAttached = true, want always false")
		}
	})

	t.Run("ListRunning_ReturnsError", func(t *testing.T) {
		if _, err := p.ListRunning(name); err == nil {
			t.Fatal("ListRunning = nil error, want unsupported error")
		}
	})

	t.Run("RunLive_ReturnsNil", func(t *testing.T) {
		if err := p.RunLive(name, runtime.Config{}); err != nil {
			t.Fatalf("RunLive = %v, want nil", err)
		}
	})

	t.Run("Capabilities_NoAttachment", func(t *testing.T) {
		caps := p.Capabilities()
		if caps.CanReportAttachment {
			t.Fatal("CanReportAttachment = true, want always false")
		}
	})

	// ── Metadata ─────────────────────────────────────────────────────────────

	t.Run("Meta_SetGetRemove", func(t *testing.T) {
		const key = "live-suite-key"
		const val = "hello from live E2E"

		// initially absent — Worker returns 200 with empty value, not 404
		got, err := p.GetMeta(name, key)
		if err != nil {
			t.Fatalf("GetMeta missing: %v", err)
		}
		if got != "" {
			t.Fatalf("GetMeta missing = %q, want empty string", got)
		}

		if err := p.SetMeta(name, key, val); err != nil {
			t.Fatalf("SetMeta: %v", err)
		}

		got, err = p.GetMeta(name, key)
		if err != nil {
			t.Fatalf("GetMeta after set: %v", err)
		}
		if got != val {
			t.Fatalf("GetMeta = %q, want %q", got, val)
		}
		t.Logf("meta round-trip OK: %q = %q", key, got)

		if err := p.RemoveMeta(name, key); err != nil {
			t.Fatalf("RemoveMeta: %v", err)
		}

		got, err = p.GetMeta(name, key)
		if err != nil {
			t.Fatalf("GetMeta after remove: %v", err)
		}
		if got != "" {
			t.Fatalf("GetMeta after remove = %q, want empty string", got)
		}
		t.Log("meta remove OK")
	})

	// ── CopyTo ───────────────────────────────────────────────────────────────

	t.Run("CopyTo_FlatFile", func(t *testing.T) {
		content := []byte("hello from Go CopyTo live E2E\n")
		tmp := filepath.Join(t.TempDir(), "e2e.txt")
		if err := os.WriteFile(tmp, content, 0600); err != nil {
			t.Fatalf("write temp: %v", err)
		}
		if err := p.CopyTo(name, tmp, "e2e-live.txt"); err != nil {
			t.Fatalf("CopyTo: %v", err)
		}
		t.Log("CopyTo flat file OK")
	})

	t.Run("CopyTo_Subdir", func(t *testing.T) {
		content := []byte("subdir content\n")
		tmp := filepath.Join(t.TempDir(), "sub.txt")
		if err := os.WriteFile(tmp, content, 0600); err != nil {
			t.Fatalf("write temp: %v", err)
		}
		if err := p.CopyTo(name, tmp, "live-sub/dir/e2e.txt"); err != nil {
			t.Fatalf("CopyTo subdir: %v", err)
		}
		t.Log("CopyTo subdir OK")
	})

	t.Run("CopyTo_TraversalRejected", func(t *testing.T) {
		tmp := filepath.Join(t.TempDir(), "escape.txt")
		if err := os.WriteFile(tmp, []byte("x"), 0600); err != nil {
			t.Fatalf("write temp: %v", err)
		}
		if err := p.CopyTo(name, tmp, "../escape.txt"); err == nil {
			t.Fatal("CopyTo '../' want error, got nil")
		} else {
			t.Logf("traversal rejected: %v", err)
		}
	})

	// ── Nudge ────────────────────────────────────────────────────────────────

	t.Run("Nudge_DoesNotError", func(t *testing.T) {
		// Worker appends text to .gc-nudge and returns 200. The Go provider
		// passes out=nil so any 2xx is success.
		if err := p.Nudge(name, runtime.TextContent("hello nudge E2E")); err != nil {
			t.Fatalf("Nudge: %v", err)
		}
		t.Log("Nudge OK")
	})

	// ── Peek + ClearScrollback ────────────────────────────────────────────────
	// Peek reads .gc-scrollback via tail. CopyTo can write that file directly,
	// letting us control the content without needing an exec API.

	t.Run("Peek_ReadsScrollback", func(t *testing.T) {
		content := []byte("peek-line-1\npeek-line-2\n")
		tmp := filepath.Join(t.TempDir(), "scrollback.txt")
		if err := os.WriteFile(tmp, content, 0600); err != nil {
			t.Fatalf("write temp: %v", err)
		}
		if err := p.CopyTo(name, tmp, ".gc-scrollback"); err != nil {
			t.Fatalf("CopyTo .gc-scrollback: %v", err)
		}

		out, err := p.Peek(name, 5)
		if err != nil {
			t.Fatalf("Peek: %v", err)
		}
		if !strings.Contains(out, "peek-line-1") {
			t.Fatalf("Peek output = %q, want to contain peek-line-1", out)
		}
		t.Logf("Peek OK: %q", out)
	})

	t.Run("ClearScrollback_EmptiesBuffer", func(t *testing.T) {
		// Ensure there is content first.
		content := []byte("to-be-cleared\n")
		tmp := filepath.Join(t.TempDir(), "sb.txt")
		if err := os.WriteFile(tmp, content, 0600); err != nil {
			t.Fatalf("write temp: %v", err)
		}
		if err := p.CopyTo(name, tmp, ".gc-scrollback"); err != nil {
			t.Fatalf("CopyTo .gc-scrollback: %v", err)
		}

		if err := p.ClearScrollback(name); err != nil {
			t.Fatalf("ClearScrollback: %v", err)
		}

		out, err := p.Peek(name, 5)
		if err != nil {
			t.Fatalf("Peek after clear: %v", err)
		}
		if strings.TrimSpace(out) != "" {
			t.Fatalf("Peek after ClearScrollback = %q, want empty", out)
		}
		t.Log("ClearScrollback OK")
	})

	// ── ProcessAlive ─────────────────────────────────────────────────────────

	t.Run("ProcessAlive_FalseForAbsentProcess", func(t *testing.T) {
		// No persistent process was started; a made-up name should be absent.
		if p.ProcessAlive(name, []string{"definitely-absent-process-xyz"}) {
			t.Fatal("ProcessAlive for absent process = true, want false")
		}
		t.Log("ProcessAlive absent OK")
	})

	t.Run("ProcessAlive_TrueForEmptyNames", func(t *testing.T) {
		if !p.ProcessAlive(name, nil) {
			t.Fatal("ProcessAlive nil names = false, want true (early return)")
		}
	})

	// ── Interrupt ────────────────────────────────────────────────────────────

	t.Run("Interrupt_DoesNotError", func(t *testing.T) {
		// pkill -INT -u $(id -u) is best-effort; with no user processes running
		// it exits 1 (no match), but the command uses `2>/dev/null; true` so
		// exec succeeds. The Go provider returns nil on 2xx.
		if err := p.Interrupt(name); err != nil {
			t.Fatalf("Interrupt: %v", err)
		}
		t.Log("Interrupt OK")
	})

	// ── SendKeys (flagged 501 endpoint) ──────────────────────────────────────

	t.Run("SendKeys_Returns501Error", func(t *testing.T) {
		// The Worker returns 501 "keys not wired in M3" — PTY proxy not yet
		// implemented. The Go provider maps any non-2xx non-404 to a plain error.
		err := p.SendKeys(name, "Enter")
		if err == nil {
			t.Fatal("SendKeys = nil, want error (Worker returns 501 flagged endpoint)")
		}
		t.Logf("SendKeys correctly returns error: %v", err)
	})

	// ── Stop ─────────────────────────────────────────────────────────────────
	// Run last: stops the shared session. The t.Cleanup above does a best-effort
	// second stop, which is idempotent (404 → nil).

	t.Run("Stop_SessionNoLongerRunning", func(t *testing.T) {
		if err := p.Stop(name); err != nil {
			t.Fatalf("Stop: %v", err)
		}
		t.Log("Stop OK")

		// The Worker deregisters immediately; no sleep needed.
		if p.IsRunning(name) {
			t.Fatal("IsRunning = true after Stop, want false")
		}
		t.Log("IsRunning after Stop = false OK")
	})
}
