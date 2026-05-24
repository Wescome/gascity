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

// TestLiveCopyTo runs a full end-to-end CopyTo test against the real deployed
// Cloudflare Worker + Sandbox. Requires GC_CLOUDFLARE_RUNTIME_URL (and
// optionally GC_CLOUDFLARE_RUNTIME_TOKEN) to be set.
//
// Run with:
//
//	GC_CLOUDFLARE_RUNTIME_URL=https://... go test -v -run TestLiveCopyTo ./internal/runtime/cloudflare/
func TestLiveCopyTo(t *testing.T) {
	endpoint := os.Getenv("GC_CLOUDFLARE_RUNTIME_URL")
	if endpoint == "" {
		t.Skip("GC_CLOUDFLARE_RUNTIME_URL not set — skipping live integration test")
	}

	p, err := NewProvider()
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}

	sessionName := fmt.Sprintf("live-e2e-%d", time.Now().UnixNano())
	t.Logf("session: %s", sessionName)

	if err := p.Start(context.Background(), sessionName, runtime.Config{WorkDir: "/workspace"}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = p.Stop(sessionName) })

	// --- flat file ---
	content := []byte("hello from Go CopyTo live E2E\n")
	tmpFile := filepath.Join(t.TempDir(), "e2e.txt")
	if err := os.WriteFile(tmpFile, content, 0600); err != nil {
		t.Fatalf("write temp file: %v", err)
	}

	if err := p.CopyTo(sessionName, tmpFile, "e2e.txt"); err != nil {
		t.Fatalf("CopyTo flat file: %v", err)
	}

	type execOut struct {
		Stdout string `json:"stdout"`
	}

	// verify via exec — sandbox exec trims trailing newline from stdout, so
	// compare byte count separately and content sans-trailing-newline.
	wantText := strings.TrimRight(string(content), "\n")
	wantBytes := fmt.Sprintf("%d", len(content))

	var out execOut
	if err := p.exec(context.Background(), sessionName, "cat /workspace/e2e.txt", &out); err != nil {
		t.Fatalf("exec cat flat file: %v", err)
	}
	if out.Stdout != wantText {
		t.Fatalf("flat file content mismatch\n got:  %q\nwant: %q", out.Stdout, wantText)
	}

	var wcOut execOut
	if err := p.exec(context.Background(), sessionName, "wc -c < /workspace/e2e.txt | tr -d ' '", &wcOut); err != nil {
		t.Fatalf("exec wc flat file: %v", err)
	}
	if strings.TrimSpace(wcOut.Stdout) != wantBytes {
		t.Fatalf("flat file byte count mismatch: got %q want %q", wcOut.Stdout, wantBytes)
	}
	t.Logf("flat file OK: content=%q bytes=%s", out.Stdout, wcOut.Stdout)

	// --- subdirectory ---
	if err := p.CopyTo(sessionName, tmpFile, "sub/dir/e2e.txt"); err != nil {
		t.Fatalf("CopyTo subdir: %v", err)
	}
	var out2 execOut
	if err := p.exec(context.Background(), sessionName, "cat /workspace/sub/dir/e2e.txt", &out2); err != nil {
		t.Fatalf("exec cat subdir: %v", err)
	}
	if out2.Stdout != wantText {
		t.Fatalf("subdir content mismatch\n got:  %q\nwant: %q", out2.Stdout, wantText)
	}
	t.Logf("subdir OK: %q", out2.Stdout)

	// --- path traversal must be rejected ---
	if err := p.CopyTo(sessionName, tmpFile, "../escape.txt"); err == nil {
		t.Fatal("CopyTo with '../' should be rejected but returned nil")
	} else {
		t.Logf("traversal correctly rejected: %v", err)
	}
}
