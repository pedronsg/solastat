package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"solastat/internal/mqttclient"
)

func writeTestProfile(t *testing.T, dir, id, name string) {
	t.Helper()
	data := `{"name":"` + name + `","modbus":{"slave_address":1,"poll_interval_ms":5000},"registers":[]}`
	if err := os.WriteFile(filepath.Join(dir, id+".json"), []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}

func testState() configState {
	return configState{
		ActiveProfile: "swatten",
		UARTPort:      "ttyAMA0",
		Timezone:      "Europe/Lisbon",
		SetupDone:     true,
		Notifications: NotificationsConfig{Enabled: true, BatteryLowThreshold: 15},
		MQTT:          mqttclient.Config{Enabled: true, Broker: "192.168.1.10", Port: 1883, Username: "ha", Password: "secret"},
	}
}

func TestConfigBackupRoundTrip(t *testing.T) {
	src := t.TempDir()
	writeTestProfile(t, src, "swatten", "Swatten")
	writeTestProfile(t, src, "my_deye", "My Deye")

	b, err := buildConfigBackup(src, "1.2.3", testState())
	if err != nil {
		t.Fatal(err)
	}
	if len(b.Profiles) != 2 {
		t.Fatalf("got %d profiles, want 2", len(b.Profiles))
	}
	if b.MQTT.Password != "secret" {
		t.Fatal("the backup should include the MQTT password")
	}

	data, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	var back configBackup
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatal(err)
	}
	if err := back.validate(); err != nil {
		t.Fatalf("a fresh backup should validate: %v", err)
	}

	dst := t.TempDir()
	if err := back.writeProfiles(dst); err != nil {
		t.Fatal(err)
	}
	p, err := loadProfile("my_deye", dst)
	if err != nil || p.Name != "My Deye" {
		t.Fatalf("restored profile = %v, %v", p, err)
	}
}

func TestConfigBackupValidateRejects(t *testing.T) {
	good := func() configBackup {
		return configBackup{Format: configBackupFormat, Version: 1, Timezone: "Europe/Lisbon",
			Profiles: map[string]json.RawMessage{"swatten": json.RawMessage(`{"name":"Swatten"}`)}}
	}
	cases := map[string]func(*configBackup){
		"another file":       func(b *configBackup) { b.Format = "something-else" },
		"newer version":      func(b *configBackup) { b.Version = configBackupVersion + 1 },
		"unknown timezone":   func(b *configBackup) { b.Timezone = "Mars/Olympus" },
		"path in profile id": func(b *configBackup) { b.Profiles["../etc/passwd"] = json.RawMessage(`{}`) },
		"broken profile":     func(b *configBackup) { b.Profiles["x"] = json.RawMessage(`{"name": 5}`) },
		"bad MQTT port":      func(b *configBackup) { b.MQTT.Port = 70000 },
		"bad active profile": func(b *configBackup) { b.ActiveProfile = "a/b" },
	}
	g := good()
	if err := g.validate(); err != nil {
		t.Fatalf("baseline should validate: %v", err)
	}
	for name, mutate := range cases {
		b := good()
		mutate(&b)
		if err := b.validate(); err == nil {
			t.Errorf("%s: expected validate to fail", name)
		} else if strings.TrimSpace(err.Error()) == "" {
			t.Errorf("%s: empty error", name)
		}
	}
}

// hubDirs makes a device's data dir and its inverters/ folder, as main() does.
func hubDirs(t *testing.T) (dataDir, inverterDir string) {
	t.Helper()
	dataDir = t.TempDir()
	inverterDir = filepath.Join(dataDir, invertersDirName)
	if err := os.MkdirAll(inverterDir, 0o755); err != nil {
		t.Fatal(err)
	}
	return dataDir, inverterDir
}

// stateOf reads a device's configuration back from its settings store, the way
// the /api/backup handler does.
func stateOf(st *settingsStore) configState {
	s := st.Get()
	return configState{ActiveProfile: s.ActiveProfile, UARTPort: s.UARTPort, Timezone: s.Timezone,
		SetupDone: s.SetupDone, Notifications: s.Notifications, MQTT: s.MQTT}
}

// throughFile marshals and re-reads a backup, as a download and upload would.
func throughFile(t *testing.T, b configBackup) configBackup {
	t.Helper()
	data, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	var out configBackup
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	if err := out.validate(); err != nil {
		t.Fatalf("backup should validate: %v", err)
	}
	return out
}

func TestRestoreEndToEnd(t *testing.T) {
	// Device A, set up: a custom profile active on ttyUSB0, Madrid, MQTT.
	dataA, dirA := hubDirs(t)
	writeTestProfile(t, dirA, "swatten", "Swatten")
	writeTestProfile(t, dirA, "my_deye", "My Deye")
	stA := openSettings(dataA)
	if err := stA.Update(func(s *appSettings) {
		s.ActiveProfile, s.UARTPort, s.Timezone, s.SetupDone = "my_deye", "ttyUSB0", "Europe/Madrid", true
		s.Notifications.BatteryLowThreshold = 15
		s.MQTT.Enabled, s.MQTT.Broker, s.MQTT.Password = true, "192.168.1.10", "secretA"
	}); err != nil {
		t.Fatal(err)
	}
	backupA, err := buildConfigBackup(dirA, "1.0.0", stateOf(stA))
	if err != nil {
		t.Fatal(err)
	}
	backupA = throughFile(t, backupA)

	// Device B: another device with its own settings and only ttyAMA0.
	dataB, dirB := hubDirs(t)
	writeTestProfile(t, dirB, "swatten", "Swatten")
	stB := openSettings(dataB)
	if err := stB.Update(func(s *appSettings) {
		s.UARTPort, s.Timezone = "ttyAMA0", "Europe/Lisbon"
		s.MQTT.Broker, s.MQTT.Password = "10.0.0.2", "secretB"
	}); err != nil {
		t.Fatal(err)
	}
	beforeRestore, err := buildConfigBackup(dirB, "1.0.0", stateOf(stB)) // what /api/restore saves first
	if err != nil {
		t.Fatal(err)
	}
	beforeRestore = throughFile(t, beforeRestore)

	port, warnings, err := backupA.apply(dirB, stB, []string{"ttyAMA0"})
	if err != nil {
		t.Fatal(err)
	}
	if port != "" || len(warnings) != 1 || !strings.Contains(warnings[0], "ttyUSB0") {
		t.Fatalf("port = %q, warnings = %v: the missing ttyUSB0 should be a warning", port, warnings)
	}

	// "Restart" Device B: everything must come back from disk.
	got := openSettings(dataB).Get()
	if got.ActiveProfile != "my_deye" || got.Timezone != "Europe/Madrid" || !got.SetupDone || got.Notifications.BatteryLowThreshold != 15 {
		t.Fatalf("restored settings = %+v", got)
	}
	if got.MQTT.Broker != "192.168.1.10" || got.MQTT.Password != "secretA" {
		t.Fatalf("MQTT = %+v: broker and password from the backup", got.MQTT)
	}
	if got.UARTPort != "ttyAMA0" {
		t.Fatalf("UART port = %q, want this device's ttyAMA0 kept", got.UARTPort)
	}
	if p, err := loadProfile("my_deye", dirB); err != nil || p.Name != "My Deye" {
		t.Fatalf("custom profile not restored: %v %v", p, err)
	}

	// Undo with the configuration saved before the restore.
	if _, _, err := beforeRestore.apply(dirB, stB, []string{"ttyAMA0"}); err != nil {
		t.Fatal(err)
	}
	undone := openSettings(dataB).Get()
	if undone.Timezone != "Europe/Lisbon" || undone.MQTT.Broker != "10.0.0.2" || undone.MQTT.Password != "secretB" {
		t.Fatalf("after undo = %+v", undone)
	}
}

func TestRestoreReplacesTheMQTTPassword(t *testing.T) {
	dirA := t.TempDir()
	writeTestProfile(t, dirA, "swatten", "Swatten")
	backup, err := buildConfigBackup(dirA, "", testState()) // testState's password: "secret"
	if err != nil {
		t.Fatal(err)
	}
	backup = throughFile(t, backup)

	dataB, dirB := hubDirs(t)
	stB := openSettings(dataB)
	_ = stB.Update(func(s *appSettings) { s.MQTT.Password = "old" })
	if _, _, err := backup.apply(dirB, stB, []string{"ttyAMA0"}); err != nil {
		t.Fatal(err)
	}
	if got := openSettings(dataB).Get(); got.MQTT.Password != "secret" || got.UARTPort != "ttyAMA0" {
		t.Fatalf("password = %q port = %q; want the backup's password and its port (present here)", got.MQTT.Password, got.UARTPort)
	}
}
