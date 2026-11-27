// Package pluginhub is the core's key store and plugin registry — not a
// hub in the networked sense any more: every plugin is compiled directly
// into this binary (from the app's extension, solastat/ext), so there's
// no process boundary and no RPC left to broker.
//
// The core is deliberately NOT the authority on plugin licensing: it only
// stores whatever key the user pastes into Settings (opaque to it — it
// never parses or verifies one) and hands the raw pool to whichever
// compiled-in plugins ask for it via Keys(). Each plugin checks its own
// key and registers an Info provider
// reporting whether it considers itself authorized; a plugin that never
// reports authorized=true is not shown here at all — there is no fixed
// catalog, and an uncompiled plugin doesn't exist to this package at all.
//
// The core only needs to display an opaque hash and store opaque strings,
// never to parse or check a key, so it builds with only solastat-template.
package pluginhub

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/OrbitOS-org/orbit-os-sdk-go/v26/logger"

	"solastat/internal/fsutil"
)

const logTag = "pluginhub"

// Info matches solastat-template's pluginapi.Info shape — duplicated here
// (rather than imported) so this always-built package stays free of any
// dependency beyond the SDK; every build-tag-gated plugin file converts its
// own pluginapi.Info into this when registering.
type Info struct {
	ID          string            `json:"id"`
	Version     string            `json:"version,omitempty"`
	Authorized  bool              `json:"authorized"`
	Name        map[string]string `json:"name,omitempty"`
	Description map[string]string `json:"description,omitempty"`
	Icon        string            `json:"icon,omitempty"`
	Category    string            `json:"category,omitempty"`
	Path        string            `json:"path,omitempty"` // the page's address; "/<ID>" when empty
}

// Store holds the raw license key pool pasted into Settings, and the
// registry of compiled-in plugins that report their status against it.
type Store struct {
	deviceSerial string
	keysPath     string

	mu   sync.RWMutex
	keys []string // raw, opaque key strings

	infoMu    sync.Mutex
	infoFuncs []func() Info
}

// New loads any previously pasted keys from keysPath.
func New(deviceSerial, keysPath string) *Store {
	s := &Store{deviceSerial: deviceSerial, keysPath: keysPath}
	if err := os.MkdirAll(filepath.Dir(keysPath), 0o755); err != nil {
		logger.Errorf(logTag, "create keys dir: %v", err)
	}
	s.keys = readKeysFile(keysPath)
	return s
}

// Keys returns a snapshot of the current raw key pool — this is what a
// compiled-in plugin calls to check itself against, on whatever schedule it
// likes (e.g. once per poll cycle).
func (s *Store) Keys() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]string{}, s.keys...)
}

// RegisterInfo adds a plugin's status provider, called on every GET
// /api/plugins. Called once at startup by each compiled-in plugin's
// build-tag-gated wiring file — never by the base package itself.
func (s *Store) RegisterInfo(f func() Info) {
	s.infoMu.Lock()
	s.infoFuncs = append(s.infoFuncs, f)
	s.infoMu.Unlock()
}

// Status returns every compiled-in plugin that currently reports itself
// authorized. A plugin nobody has a key for — or that was never compiled in
// — is simply absent, there's no fixed catalog or "locked" placeholder.
func (s *Store) Status() []Info {
	s.infoMu.Lock()
	fns := append([]func() Info{}, s.infoFuncs...)
	s.infoMu.Unlock()

	out := make([]Info, 0, len(fns))
	for _, f := range fns {
		if info := f(); info.Authorized {
			out = append(out, info)
		}
	}
	return out
}

// DeviceHash returns the hash shown (and copied) on the Settings page,
// the one a key is issued for.
func (s *Store) DeviceHash() string {
	sum := sha256.Sum256([]byte(s.deviceSerial))
	return hex.EncodeToString(sum[:])
}

// Activate appends key to the stored pool and persists it. The core does
// not verify it — every compiled-in plugin re-checks Keys() on its own
// schedule, and decides for itself whether the key names it.
func (s *Store) Activate(key string) error {
	if key == "" {
		return fmt.Errorf("key must not be empty")
	}

	s.mu.Lock()
	for _, existing := range s.keys {
		if existing == key {
			s.mu.Unlock()
			return nil // already stored
		}
	}
	s.keys = append(s.keys, key)
	keys := append([]string{}, s.keys...)
	s.mu.Unlock()

	if err := writeKeysFile(s.keysPath, keys); err != nil {
		return fmt.Errorf("persist key: %w", err)
	}
	logger.Infof(logTag, "stored a new license key (%d total)", len(keys))
	return nil
}

// Deactivate removes key from the stored pool and persists the change —
// the reverse of Activate. A plugin unlocked by it re-checks Keys() on its
// own schedule and stops reporting itself authorized once it no longer
// finds a matching key there; the core doesn't need to tell it anything.
// A key that isn't in the pool is a no-op, not an error (matches
// Activate's idempotency for a key already stored).
func (s *Store) Deactivate(key string) error {
	if key == "" {
		return fmt.Errorf("key must not be empty")
	}

	s.mu.Lock()
	idx := -1
	for i, existing := range s.keys {
		if existing == key {
			idx = i
			break
		}
	}
	if idx == -1 {
		s.mu.Unlock()
		return nil
	}
	s.keys = append(s.keys[:idx], s.keys[idx+1:]...)
	keys := append([]string{}, s.keys...)
	s.mu.Unlock()

	if err := writeKeysFile(s.keysPath, keys); err != nil {
		return fmt.Errorf("persist key removal: %w", err)
	}
	logger.Infof(logTag, "removed a license key (%d remaining)", len(keys))
	return nil
}

func readKeysFile(path string) []string {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var rec struct {
		Keys []string `json:"keys"`
	}
	if err := json.Unmarshal(b, &rec); err != nil {
		return nil
	}
	return rec.Keys
}

func writeKeysFile(path string, keys []string) error {
	b, err := json.MarshalIndent(struct {
		Keys []string `json:"keys"`
	}{Keys: keys}, "", "  ")
	if err != nil {
		return err
	}
	return fsutil.WriteFileAtomic(path, b, 0o644)
}
