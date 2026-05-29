// Package noop implements a no-op [runtime.Provider] for harness-executed
// formula steps. It satisfies the session lifecycle contract (Start/IsRunning/
// Stop/ListRunning) via local state files so the Gas City reconciler can
// track sessions without starting any external service or remote Sandbox.
//
// Use this provider for city agents whose steps all declare
// runtime_requirements and are executed by the harness registry (e.g. pi-rpc).
// The session is a lifecycle marker only — no process is spawned, no remote
// endpoint is contacted, and no cost is incurred.
package noop

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

const defaultStateDir = "/tmp/gc-noop-sessions"

// Provider is the no-op session provider.
type Provider struct {
	mu  sync.Mutex
	dir string
}

var _ runtime.Provider = (*Provider)(nil)

// NewProvider returns a Provider using a default temporary state directory.
func NewProvider() *Provider {
	return NewProviderWithDir(defaultStateDir)
}

// NewProviderWithDir returns a Provider using dir for state files.
func NewProviderWithDir(dir string) *Provider {
	return &Provider{dir: dir}
}

type sessionState struct {
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
}

// stateFile returns the path for a session's state file.
func (p *Provider) stateFile(name string) string {
	sum := sha256.Sum256([]byte(name))
	return filepath.Join(p.dir, hex.EncodeToString(sum[:16])+".json")
}

func (p *Provider) ensureDir() error {
	return os.MkdirAll(p.dir, 0o700)
}

// Start writes a state file for name. Returns ErrSessionExists if already running.
func (p *Provider) Start(_ context.Context, name string, _ runtime.Config) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.ensureDir(); err != nil {
		return fmt.Errorf("noop provider: mkdir %s: %w", p.dir, err)
	}
	path := p.stateFile(name)
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("%w: noop session %q", runtime.ErrSessionExists, name)
	}
	data, err := json.Marshal(sessionState{Name: name, CreatedAt: time.Now().UTC()})
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}

// Stop removes the state file. Idempotent.
func (p *Provider) Stop(name string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := os.Remove(p.stateFile(name)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// Interrupt is a no-op — there is no process to signal.
func (p *Provider) Interrupt(_ string) error { return nil }

// IsRunning reports whether a state file exists for name.
func (p *Provider) IsRunning(name string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	_, err := os.Stat(p.stateFile(name))
	return err == nil
}

// IsAttached always returns false.
func (p *Provider) IsAttached(_ string) bool { return false }

// Attach is not supported.
func (p *Provider) Attach(_ string) error {
	return fmt.Errorf("noop provider does not support attach")
}

// ProcessAlive returns true when processNames is empty (no process to check).
func (p *Provider) ProcessAlive(_ string, processNames []string) bool {
	return len(processNames) == 0
}

// Nudge is a no-op.
func (p *Provider) Nudge(_ string, _ []runtime.ContentBlock) error { return nil }

// SetMeta is a no-op.
func (p *Provider) SetMeta(_, _, _ string) error { return nil }

// GetMeta always returns ("", nil).
func (p *Provider) GetMeta(_, _ string) (string, error) { return "", nil }

// RemoveMeta is a no-op.
func (p *Provider) RemoveMeta(_, _ string) error { return nil }

// Peek returns empty output.
func (p *Provider) Peek(_ string, _ int) (string, error) { return "", nil }

// ListRunning returns the names of all sessions whose names have the given prefix.
func (p *Provider) ListRunning(prefix string) ([]string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	entries, err := os.ReadDir(p.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(p.dir, e.Name()))
		if err != nil {
			continue
		}
		var s sessionState
		if err := json.Unmarshal(data, &s); err != nil {
			continue
		}
		if prefix == "" || strings.HasPrefix(s.Name, prefix) {
			names = append(names, s.Name)
		}
	}
	return names, nil
}

// GetLastActivity returns the creation time stored in the state file.
func (p *Provider) GetLastActivity(name string) (time.Time, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	data, err := os.ReadFile(p.stateFile(name))
	if err != nil {
		return time.Time{}, nil
	}
	var s sessionState
	if err := json.Unmarshal(data, &s); err != nil {
		return time.Time{}, nil
	}
	return s.CreatedAt, nil
}

// ClearScrollback is a no-op.
func (p *Provider) ClearScrollback(_ string) error { return nil }

// CopyTo is a no-op.
func (p *Provider) CopyTo(_, _, _ string) error { return nil }

// SendKeys is a no-op.
func (p *Provider) SendKeys(_ string, _ ...string) error { return nil }

// RunLive is a no-op.
func (p *Provider) RunLive(_ string, _ runtime.Config) error { return nil }

// Capabilities returns zero capabilities — noop sessions have no observable state.
func (p *Provider) Capabilities() runtime.ProviderCapabilities {
	return runtime.ProviderCapabilities{}
}
