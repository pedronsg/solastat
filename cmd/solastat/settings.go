package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/OrbitOS-org/orbit-os-sdk-go/v26/logger"

	"solastat/internal/fsutil"
	"solastat/internal/mqttclient"
)

// settings.json holds the app's own settings in one versioned file, written
// atomically (see fsutil.WriteFileAtomic) so a power cut never leaves it
// half-written. Inverter profiles (one JSON file each), the history
// databases, license keys and each plugin's own settings stay in their own
// files.
const (
	settingsFileName = "settings.json"
	settingsVersion  = 1
	// configPreRestoreFile is the configuration saved just before a restore.
	configPreRestoreFile = "config_pre_restore.json"
	// invertersDirName is the data dir's folder of inverter profiles.
	invertersDirName = "inverters"
)

type appSettings struct {
	Version       int    `json:"version"`
	ActiveProfile string `json:"active_profile,omitempty"`
	UARTPort      string `json:"uart_port,omitempty"`
	Timezone      string `json:"timezone,omitempty"`
	// Language is the one last picked in the web UI (its code: en, pt, ...),
	// for what the server shows by itself (a board's screen); the pages
	// keep their own copy in the browser. Empty is English.
	Language string `json:"language,omitempty"`
	// MonthStartDay is the day (1–28) each month view starts on, so a month
	// can follow a billing cycle (5 Jan – 4 Feb); 1 is the calendar month.
	MonthStartDay int                 `json:"month_start_day,omitempty"`
	SetupDone     bool                `json:"setup_done"`
	Notifications NotificationsConfig `json:"notifications"`
	MQTT          mqttclient.Config   `json:"mqtt"`
}

func defaultSettings() appSettings {
	return appSettings{
		Version:       settingsVersion,
		MonthStartDay: 1,
		Notifications: DefaultNotificationsConfig(),
		MQTT:          mqttclient.DefaultConfig(),
	}
}

// clone returns a copy that shares no map with s.
func (s appSettings) clone() appSettings {
	pt := make(map[string]bool, len(s.Notifications.PluginTypes))
	for k, v := range s.Notifications.PluginTypes {
		pt[k] = v
	}
	s.Notifications.PluginTypes = pt
	return s
}

// maxMonthStartDay keeps every month's cycle start a day that month has.
const maxMonthStartDay = 28

// monthPeriod is the month (cycle) holding anchor when months start on
// startDay: [start, end), end being the same day a month later.
func monthPeriod(anchor time.Time, startDay int, loc *time.Location) (start, end time.Time) {
	if startDay < 1 || startDay > maxMonthStartDay {
		startDay = 1
	}
	y, m, d := anchor.Date()
	if d < startDay {
		m--
	}
	start = time.Date(y, m, startDay, 0, 0, 0, 0, loc)
	return start, start.AddDate(0, 1, 0)
}

// settingsStore is the in-memory copy of settings.json; every change goes
// through Update, which saves the file before the new values are used.
type settingsStore struct {
	path string
	mu   sync.RWMutex
	s    appSettings
}

// openSettings loads dir/settings.json (the defaults when there is none
// yet). It never fails: an unreadable file is kept aside as
// settings.json.bad and logged, and the defaults are used.
func openSettings(dir string) *settingsStore {
	st := &settingsStore{path: filepath.Join(dir, settingsFileName), s: defaultSettings()}

	data, err := os.ReadFile(st.path)
	switch {
	case err == nil:
		s := defaultSettings()
		if err := json.Unmarshal(data, &s); err != nil {
			bad := st.path + ".bad"
			logger.Errorf(logTag, "settings: %s is unreadable (%v); kept as %s, starting from the defaults", st.path, err, bad)
			_ = os.Rename(st.path, bad)
		} else {
			st.s = normalizeSettings(s)
			return st
		}
	case !os.IsNotExist(err):
		logger.Errorf(logTag, "settings: read %s: %v; starting from the defaults", st.path, err)
	}
	return st
}

// normalizeSettings fills what a file may lack.
func normalizeSettings(s appSettings) appSettings {
	s.Version = settingsVersion
	if s.MonthStartDay < 1 || s.MonthStartDay > maxMonthStartDay {
		s.MonthStartDay = 1
	}
	if s.Notifications.BatteryLowThreshold <= 0 {
		s.Notifications.BatteryLowThreshold = DefaultNotificationsConfig().BatteryLowThreshold
	}
	if s.Notifications.PluginTypes == nil {
		s.Notifications.PluginTypes = map[string]bool{}
	}
	return s
}

// Get returns a copy of the current settings.
func (st *settingsStore) Get() appSettings {
	st.mu.RLock()
	defer st.mu.RUnlock()
	return st.s.clone()
}

// Update applies fn to a copy of the settings and saves it; the new values
// are only used once the file is safely written.
func (st *settingsStore) Update(fn func(*appSettings)) error {
	st.mu.Lock()
	defer st.mu.Unlock()
	next := st.s.clone()
	fn(&next)
	next = normalizeSettings(next)
	if err := st.save(next); err != nil {
		return err
	}
	st.s = next
	return nil
}

func (st *settingsStore) save(s appSettings) error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	// 0600: it holds the MQTT password.
	if err := fsutil.WriteFileAtomic(st.path, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("save settings: %w", err)
	}
	return nil
}

// deleteForReset removes from disk what a reset to a fresh install
// deletes: the history database and its WAL files, every inverter profile
// (the built-in ones are then seeded again) and the configuration saved
// before a restore. settings.json is reset by the caller, through the
// store; license keys, the Wi-Fi marker and plugin files are left alone
// (plugins reset their own). Returns what it couldn't do.
func deleteForReset(dataDir, inverterDir, historyPath string) (warnings []string) {
	remove := func(path string) {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			warnings = append(warnings, fmt.Sprintf("delete %s: %v", filepath.Base(path), err))
		}
	}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		remove(historyPath + suffix)
	}
	if entries, err := os.ReadDir(inverterDir); err == nil {
		for _, e := range entries {
			if !e.IsDir() && filepath.Ext(e.Name()) == ".json" {
				remove(filepath.Join(inverterDir, e.Name()))
			}
		}
	}
	if err := ensureDefaultProfiles(inverterDir); err != nil {
		warnings = append(warnings, "restore built-in profiles: "+err.Error())
	}
	remove(filepath.Join(dataDir, configPreRestoreFile))
	return warnings
}
