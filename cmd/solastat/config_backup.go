package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"solastat/internal/mqttclient"
	"solastat/internal/profile"

	"solastat/internal/fsutil"
)

// A configuration backup is one JSON file with everything needed to set a
// device up again the same way: the inverter profiles (they hold the
// connection too: slave address, Solarman host and serial), which one is
// active and on which UART port, the timezone, notifications and MQTT.
//
// Not included: the history (exported as CSV instead), license keys (tied
// to the device they were issued for), Wi-Fi (managed by Orbit OS) and
// plugin settings (a plugin only reads its own on start).
const (
	configBackupFormat  = "solastat-config"
	configBackupVersion = 1
	// maxConfigBackupBytes bounds an uploaded backup; real ones are a few KB.
	maxConfigBackupBytes = 2 << 20
)

type configBackup struct {
	Format     string    `json:"format"`
	Version    int       `json:"version"`
	Created    time.Time `json:"created"`
	AppVersion string    `json:"app_version,omitempty"`

	ActiveProfile string `json:"active_profile"`
	UARTPort      string `json:"uart_port,omitempty"`
	Timezone      string `json:"timezone,omitempty"`
	MonthStartDay int    `json:"month_start_day,omitempty"`
	SetupDone     bool   `json:"setup_done"`

	Notifications NotificationsConfig `json:"notifications"`
	// MQTT includes its password: a restore sets the device up completely.
	MQTT mqttclient.Config `json:"mqtt"`

	// Profiles holds every profile file of the device, by id, as stored.
	Profiles map[string]json.RawMessage `json:"profiles"`
}

// configState is the device's current configuration, as main() holds it.
type configState struct {
	ActiveProfile string
	UARTPort      string
	Timezone      string
	MonthStartDay int
	SetupDone     bool
	Notifications NotificationsConfig
	MQTT          mqttclient.Config
}

// buildConfigBackup snapshots st plus every profile in profilesDir (the
// data dir's inverters/ folder).
func buildConfigBackup(profilesDir, appVersion string, st configState) (configBackup, error) {
	b := configBackup{
		Format:        configBackupFormat,
		Version:       configBackupVersion,
		Created:       time.Now().UTC().Truncate(time.Second),
		AppVersion:    appVersion,
		ActiveProfile: st.ActiveProfile,
		UARTPort:      st.UARTPort,
		Timezone:      st.Timezone,
		MonthStartDay: st.MonthStartDay,
		SetupDone:     st.SetupDone,
		Notifications: st.Notifications,
		MQTT:          st.MQTT,
		Profiles:      map[string]json.RawMessage{},
	}

	entries, err := os.ReadDir(profilesDir)
	if err != nil {
		return b, fmt.Errorf("read profiles: %w", err)
	}
	for _, e := range entries {
		id := strings.TrimSuffix(e.Name(), ".json")
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") || !profileIDPattern.MatchString(id) {
			continue
		}
		data, err := os.ReadFile(filepath.Join(profilesDir, e.Name()))
		if err != nil {
			return b, fmt.Errorf("read profile %s: %w", id, err)
		}
		if _, err := profile.Load(data); err != nil {
			continue // not a profile a restore could use
		}
		b.Profiles[id] = json.RawMessage(data)
	}
	return b, nil
}

// validate checks everything a restore would apply, so a bad file is
// refused before anything changes.
func (b *configBackup) validate() error {
	if b.Format != configBackupFormat {
		return fmt.Errorf("not a SolaStat configuration backup")
	}
	if b.Version < 1 || b.Version > configBackupVersion {
		return fmt.Errorf("backup version %d is not supported by this app version (up to %d)", b.Version, configBackupVersion)
	}
	if b.Timezone != "" {
		if _, err := time.LoadLocation(b.Timezone); err != nil {
			return fmt.Errorf("unknown timezone %q", b.Timezone)
		}
	}
	if b.MQTT.Port < 0 || b.MQTT.Port > 65535 {
		return fmt.Errorf("invalid MQTT port %d", b.MQTT.Port)
	}
	for id, raw := range b.Profiles {
		if !profileIDPattern.MatchString(id) {
			return fmt.Errorf("invalid profile id %q", id)
		}
		p, err := profile.Load(raw)
		if err != nil {
			return fmt.Errorf("profile %s: %w", id, err)
		}
		if strings.TrimSpace(p.Name) == "" {
			return fmt.Errorf("profile %s has no name", id)
		}
	}
	if b.ActiveProfile != "" && !profileIDPattern.MatchString(b.ActiveProfile) {
		return fmt.Errorf("invalid active profile id %q", b.ActiveProfile)
	}
	return nil
}

// profileIDs lists the backup's profiles in a stable order.
func (b *configBackup) profileIDs() []string {
	ids := make([]string, 0, len(b.Profiles))
	for id := range b.Profiles {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// writeProfiles writes the backup's profile files into profilesDir.
func (b *configBackup) writeProfiles(profilesDir string) error {
	for _, id := range b.profileIDs() {
		path := filepath.Join(profilesDir, id+".json")
		if err := fsutil.WriteFileAtomic(path, append([]byte(strings.TrimSpace(string(b.Profiles[id]))), '\n'), 0o644); err != nil {
			return fmt.Errorf("write profile %s: %w", id, err)
		}
	}
	return nil
}

// apply writes the backup's profiles and merges its settings into st in one
// atomic save of settings.json (all or nothing). availablePorts are this
// device's UART ports: the backup's port is only taken if it is one of them.
// It returns the port taken ("" if none) and what couldn't be applied. The
// caller then puts the new settings to work (MQTT, active profile).
func (b *configBackup) apply(profilesDir string, st *settingsStore, availablePorts []string) (port string, warnings []string, err error) {
	if err := b.writeProfiles(profilesDir); err != nil {
		return "", nil, err
	}
	if b.UARTPort != "" {
		for _, p := range availablePorts {
			if p == b.UARTPort {
				port = p
				break
			}
		}
		if port == "" {
			warnings = append(warnings, fmt.Sprintf("UART port %s is not on this device; kept the current one", b.UARTPort))
		}
	}
	err = st.Update(func(s *appSettings) {
		if b.Timezone != "" {
			s.Timezone = b.Timezone
		}
		if b.MonthStartDay != 0 {
			s.MonthStartDay = b.MonthStartDay // normalizeSettings keeps it in 1–28
		}
		s.Notifications = b.Notifications
		s.MQTT = b.MQTT
		if b.SetupDone {
			s.SetupDone = true
		}
		if port != "" {
			s.UARTPort = port
		}
		if b.ActiveProfile != "" {
			s.ActiveProfile = b.ActiveProfile
		}
	})
	return port, warnings, err
}
