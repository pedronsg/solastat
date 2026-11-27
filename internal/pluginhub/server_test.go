package pluginhub

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

func TestDeviceHashIsHexSha256OfSerial(t *testing.T) {
	s := New("device-abc-123", t.TempDir()+"/keys.json")
	sum := sha256.Sum256([]byte("device-abc-123"))
	want := hex.EncodeToString(sum[:])
	if got := s.DeviceHash(); got != want {
		t.Fatalf("DeviceHash() = %q, want %q", got, want)
	}
}

// TestStoreNeverDecidesAuthorization is the core behavioral contract: a
// plugin only ever appears in Status() based on what its own registered
// Info provider reports — storing a key, by itself, decides nothing.
func TestStoreNeverDecidesAuthorization(t *testing.T) {
	s := New("device-abc-123", t.TempDir()+"/keys.json")

	if err := s.Activate("SOLA1-whatever-a-plugin-will-decode-itself"); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	if len(s.Status()) != 0 {
		t.Fatal("storing a key must not, by itself, make any plugin appear active")
	}

	authorized := false
	s.RegisterInfo(func() Info { return Info{ID: "relay", Version: "1.0.0", Authorized: authorized} })
	if len(s.Status()) != 0 {
		t.Fatal("a plugin reporting Authorized=false must not appear in Status")
	}

	authorized = true
	got := s.Status()
	if len(got) != 1 || got[0].ID != "relay" {
		t.Fatalf("Status() = %+v, want exactly [relay]", got)
	}
}

func TestKeysHandsBackTheFullPool(t *testing.T) {
	s := New("device-abc-123", t.TempDir()+"/keys.json")
	if err := s.Activate("key-one"); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	if err := s.Activate("key-two"); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	// Idempotent: re-activating the same key must not duplicate it.
	if err := s.Activate("key-one"); err != nil {
		t.Fatalf("Activate (dup): %v", err)
	}

	keys := s.Keys()
	if len(keys) != 2 {
		t.Fatalf("Keys() returned %d keys, want 2 (dedup, no verification): %v", len(keys), keys)
	}
}

func TestDeactivateRemovesOnlyTheGivenKey(t *testing.T) {
	s := New("device-abc-123", t.TempDir()+"/keys.json")
	if err := s.Activate("key-one"); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	if err := s.Activate("key-two"); err != nil {
		t.Fatalf("Activate: %v", err)
	}

	if err := s.Deactivate("key-one"); err != nil {
		t.Fatalf("Deactivate: %v", err)
	}
	keys := s.Keys()
	if len(keys) != 1 || keys[0] != "key-two" {
		t.Fatalf("Keys() = %v, want [key-two]", keys)
	}

	// A key not in the pool is a no-op, not an error.
	if err := s.Deactivate("never-activated"); err != nil {
		t.Fatalf("Deactivate (absent key): %v", err)
	}
	if len(s.Keys()) != 1 {
		t.Fatalf("Deactivate of an absent key must not touch the pool: %v", s.Keys())
	}
}

func TestKeyPoolPersistsAcrossRestart(t *testing.T) {
	path := t.TempDir() + "/keys.json"
	s1 := New("device-abc-123", path)
	if err := s1.Activate("SOLA1-persisted-key"); err != nil {
		t.Fatalf("Activate: %v", err)
	}

	s2 := New("device-abc-123", path)
	keys := s2.Keys()
	if len(keys) != 1 || keys[0] != "SOLA1-persisted-key" {
		t.Fatalf("key pool did not survive restart: %v", keys)
	}
}

func TestActivateRejectsEmptyKey(t *testing.T) {
	s := New("device-abc-123", t.TempDir()+"/keys.json")
	if err := s.Activate(""); err == nil {
		t.Fatal("expected an error for an empty key")
	}
}
