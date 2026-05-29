package noop_test

import (
	"context"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/noop"
)

func TestStartIsRunningStop(t *testing.T) {
	dir := t.TempDir()
	p := noop.NewProviderWithDir(dir)

	if p.IsRunning("s1") {
		t.Fatal("IsRunning: want false before Start")
	}
	if err := p.Start(context.Background(), "s1", runtime.Config{}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if !p.IsRunning("s1") {
		t.Fatal("IsRunning: want true after Start")
	}
	if err := p.Stop("s1"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if p.IsRunning("s1") {
		t.Fatal("IsRunning: want false after Stop")
	}
	// Idempotent stop.
	if err := p.Stop("s1"); err != nil {
		t.Fatalf("Stop (idempotent): %v", err)
	}
}

func TestStartDuplicateReturnsErrSessionExists(t *testing.T) {
	dir := t.TempDir()
	p := noop.NewProviderWithDir(dir)

	if err := p.Start(context.Background(), "s1", runtime.Config{}); err != nil {
		t.Fatalf("first Start: %v", err)
	}
	err := p.Start(context.Background(), "s1", runtime.Config{})
	if err == nil {
		t.Fatal("duplicate Start: want ErrSessionExists, got nil")
	}
	if !isSessionExists(err) {
		t.Fatalf("duplicate Start: want ErrSessionExists wrapped, got %v", err)
	}
}

func isSessionExists(err error) bool {
	return err != nil && containsString(err.Error(), "already exists")
}

func containsString(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(sub) == 0 ||
		func() bool {
			for i := 0; i <= len(s)-len(sub); i++ {
				if s[i:i+len(sub)] == sub {
					return true
				}
			}
			return false
		}())
}

func TestListRunning(t *testing.T) {
	dir := t.TempDir()
	p := noop.NewProviderWithDir(dir)

	for _, name := range []string{"alpha", "beta", "gamma"} {
		if err := p.Start(context.Background(), name, runtime.Config{}); err != nil {
			t.Fatalf("Start %s: %v", name, err)
		}
	}
	all, err := p.ListRunning("")
	if err != nil {
		t.Fatalf("ListRunning: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("ListRunning: want 3, got %d: %v", len(all), all)
	}

	prefixed, err := p.ListRunning("al")
	if err != nil {
		t.Fatalf("ListRunning prefix: %v", err)
	}
	if len(prefixed) != 1 || prefixed[0] != "alpha" {
		t.Fatalf("ListRunning prefix: want [alpha], got %v", prefixed)
	}
}

func TestGetLastActivity(t *testing.T) {
	dir := t.TempDir()
	p := noop.NewProviderWithDir(dir)

	zero, _ := p.GetLastActivity("missing")
	if !zero.IsZero() {
		t.Fatal("GetLastActivity missing: want zero time")
	}
	if err := p.Start(context.Background(), "s1", runtime.Config{}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	ts, err := p.GetLastActivity("s1")
	if err != nil {
		t.Fatalf("GetLastActivity: %v", err)
	}
	if ts.IsZero() {
		t.Fatal("GetLastActivity: want non-zero after Start")
	}
}

func TestRestartDurability(t *testing.T) {
	dir := t.TempDir()
	p1 := noop.NewProviderWithDir(dir)

	if err := p1.Start(context.Background(), "persistent", runtime.Config{}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// New provider instance from same dir — simulates control-dispatcher restart.
	p2 := noop.NewProviderWithDir(dir)
	if !p2.IsRunning("persistent") {
		t.Fatal("IsRunning after restart: want true (file-backed state)")
	}
	names, err := p2.ListRunning("")
	if err != nil {
		t.Fatalf("ListRunning after restart: %v", err)
	}
	if len(names) != 1 || names[0] != "persistent" {
		t.Fatalf("ListRunning after restart: want [persistent], got %v", names)
	}
}

func TestProcessAlive(t *testing.T) {
	p := noop.NewProviderWithDir(t.TempDir())
	// Empty processNames → true (per Provider contract).
	if !p.ProcessAlive("any", nil) {
		t.Fatal("ProcessAlive empty: want true")
	}
	// Non-empty processNames → false (no real process).
	if p.ProcessAlive("any", []string{"gc"}) {
		t.Fatal("ProcessAlive non-empty: want false")
	}
}
