package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"solastat/internal/profile"
)

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func exists(dir, name string) bool {
	_, err := os.Stat(filepath.Join(dir, name))
	return err == nil
}

func TestSettingsFreshInstallUsesDefaults(t *testing.T) {
	dir := t.TempDir()
	s := openSettings(dir).Get()
	if s.SetupDone || s.ActiveProfile != "" || !s.Notifications.Enabled || s.MQTT.Port != 1883 {
		t.Fatalf("fresh settings = %+v", s)
	}
	if exists(dir, settingsFileName) {
		t.Fatal("a fresh install shouldn't write settings.json until something changes")
	}
}

func TestSettingsFileIsPrivate(t *testing.T) {
	dir := t.TempDir()
	if err := openSettings(dir).Update(func(s *appSettings) { s.MQTT.Password = "secret" }); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(filepath.Join(dir, settingsFileName)); fi.Mode().Perm() != 0o600 {
		t.Fatalf("settings.json mode = %v, want 0600 (it holds the MQTT password)", fi.Mode().Perm())
	}
}

func TestSettingsUpdateSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	st := openSettings(dir)
	if err := st.Update(func(s *appSettings) {
		s.Timezone = "Europe/Paris"
		s.SetupDone = true
		s.Notifications.PluginTypes["smartspot.decision"] = false
	}); err != nil {
		t.Fatal(err)
	}
	s := openSettings(dir).Get()
	if s.Timezone != "Europe/Paris" || !s.SetupDone || s.Notifications.PluginTypes["smartspot.decision"] {
		t.Fatalf("settings after restart = %+v", s)
	}
}

func TestSettingsGetReturnsACopy(t *testing.T) {
	st := openSettings(t.TempDir())
	s := st.Get()
	s.Notifications.PluginTypes["x"] = true
	if _, leaked := st.Get().Notifications.PluginTypes["x"]; leaked {
		t.Fatal("changing a Get() copy changed the stored settings")
	}
}

func TestSettingsUnreadableFileIsKeptAside(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, settingsFileName, "{ not json")
	s := openSettings(dir).Get()
	if s.SetupDone || !s.Notifications.Enabled {
		t.Fatalf("expected defaults, got %+v", s)
	}
	if !exists(dir, settingsFileName+".bad") {
		t.Fatal("the unreadable file should be kept as settings.json.bad")
	}
}

func TestProfilesLiveInTheirOwnFolder(t *testing.T) {
	dataDir := t.TempDir()
	inverters := filepath.Join(dataDir, invertersDirName)
	if err := ensureDefaultProfiles(inverters); err != nil {
		t.Fatal(err)
	}
	// Everything else in the data dir must never be taken for a profile.
	writeFile(t, dataDir, settingsFileName, `{"version":1,"mqtt":{"password":"secret"}}`)
	writeFile(t, dataDir, "license_keys.json", `{"keys":["abc"]}`)
	writeFile(t, dataDir, "smartspot_schedule.json", `{"for_date":"2026-09-29T00:00:00Z"}`)

	if !exists(inverters, "swatten.json") || exists(dataDir, "swatten.json") {
		t.Fatal("built-in profiles should be seeded into inverters/, not the data dir")
	}
	list, err := listProfiles(inverters)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range list {
		if p.ID == "settings" || p.ID == "license_keys" || p.ID == "smartspot_schedule" {
			t.Fatalf("%s listed as a profile", p.ID)
		}
	}
	b, err := buildConfigBackup(inverters, "", testState())
	if err != nil {
		t.Fatal(err)
	}
	if len(b.Profiles) != len(list) || b.Profiles["swatten"] == nil {
		t.Fatalf("backup profiles = %v, want the %d built-in ones", b.profileIDs(), len(list))
	}
}

func TestDeleteForResetKeepsLicensesAndWiFi(t *testing.T) {
	dataDir, inverterDir := hubDirs(t)
	if err := ensureDefaultProfiles(inverterDir); err != nil {
		t.Fatal(err)
	}
	writeTestProfile(t, inverterDir, "my_deye", "My Deye")
	writeFile(t, inverterDir, "swatten.json", `{"name":"Swatten (edited)"}`)
	history := filepath.Join(dataDir, "history.db")
	for _, name := range []string{"history.db", "history.db-wal", "history.db-shm", configPreRestoreFile, "license_keys.json", "wifi_joined.cfg", "smartspot.cfg"} {
		writeFile(t, dataDir, name, "x")
	}

	if w := deleteForReset(dataDir, inverterDir, history); len(w) != 0 {
		t.Fatalf("warnings: %v", w)
	}
	for _, gone := range []string{"history.db", "history.db-wal", "history.db-shm", configPreRestoreFile} {
		if exists(dataDir, gone) {
			t.Errorf("%s should be deleted", gone)
		}
	}
	for _, kept := range []string{"license_keys.json", "wifi_joined.cfg", "smartspot.cfg"} {
		if !exists(dataDir, kept) {
			t.Errorf("%s should be kept (plugins reset their own files)", kept)
		}
	}
	if exists(inverterDir, "my_deye.json") {
		t.Error("custom profiles should be deleted")
	}
	p, err := loadProfile("swatten", inverterDir)
	if err != nil || p.Name == "Swatten (edited)" {
		t.Fatalf("swatten should be back to the built-in profile, got %v (%v)", p, err)
	}
}

func TestMonthPeriod(t *testing.T) {
	loc := time.UTC
	day := func(y int, m time.Month, d int) time.Time { return time.Date(y, m, d, 0, 0, 0, 0, loc) }
	cases := []struct {
		anchor     time.Time
		startDay   int
		start, end time.Time
	}{
		{day(2026, 3, 17), 1, day(2026, 3, 1), day(2026, 4, 1)},
		{day(2026, 3, 17), 0, day(2026, 3, 1), day(2026, 4, 1)},    // unset: calendar month
		{day(2026, 1, 5), 5, day(2026, 1, 5), day(2026, 2, 5)},     // the start day itself
		{day(2026, 2, 4), 5, day(2026, 1, 5), day(2026, 2, 5)},     // the cycle's last day
		{day(2026, 1, 3), 15, day(2025, 12, 15), day(2026, 1, 15)}, // across the year
		{day(2026, 3, 1), 28, day(2026, 2, 28), day(2026, 3, 28)},
		{day(2026, 3, 1), 31, day(2026, 3, 1), day(2026, 4, 1)}, // out of range: calendar month
	}
	for _, c := range cases {
		start, end := monthPeriod(c.anchor, c.startDay, loc)
		if !start.Equal(c.start) || !end.Equal(c.end) {
			t.Errorf("monthPeriod(%s, %d) = %s – %s, want %s – %s", c.anchor.Format("2006-01-02"), c.startDay,
				start.Format("2006-01-02"), end.Format("2006-01-02"), c.start.Format("2006-01-02"), c.end.Format("2006-01-02"))
		}
	}
}

func TestSettingsMonthStartDayNormalized(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, settingsFileName, `{"version":1,"month_start_day":40}`)
	if got := openSettings(dir).Get().MonthStartDay; got != 1 {
		t.Fatalf("month_start_day 40 loaded as %d, want 1", got)
	}
	writeFile(t, dir, settingsFileName, `{"version":1}`)
	if got := openSettings(dir).Get().MonthStartDay; got != 1 {
		t.Fatalf("missing month_start_day loaded as %d, want 1", got)
	}
}

func TestLoadProfileAddsNewReadingsToAnOldBuiltinCopy(t *testing.T) {
	dir := t.TempDir()
	// The Swatten profile as a device seeded it before the voltage and
	// current readings existed.
	writeFile(t, dir, "swatten.json", `{"name":"Swatten Inverter","uart":{"baud_rate":9600,"data_bits":8,"parity":"none","stop_bits":1},
		"modbus":{"slave_address":1,"poll_interval_ms":5000},
		"registers":[{"name":"pv_total_power","label":"PV","address":4067,"function_code":4,"value_type":"U_DWORD_R","unit":"W","multiplier":1},
		             {"name":"grid_voltage","label":"Mine","address":1,"function_code":4,"value_type":"U_WORD","unit":"V","multiplier":1}]}`)
	p, err := loadProfile("swatten", dir)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]int{}
	for i, r := range p.Registers {
		byName[r.Name] = i
	}
	if i, ok := byName["pv1_total_power_p_v"]; !ok || i < 2 {
		t.Fatalf("pv1 voltage missing or not appended at the end: %v", byName)
	}
	if p.Registers[byName["grid_voltage"]].Address != 1 {
		t.Fatal("a reading the copy already has must stay as the user set it")
	}
	if _, ok := byName["battery_current"]; !ok {
		t.Fatal("battery_current not added")
	}
}

// Every built-in profile loads, names each reading once, and keeps the
// voltage, current and frequency registers after the others (a failed read ends the
// poll cycle, so one a model lacks must not cost the power readings).
func TestBuiltinProfilesVoltageAndCurrentReadings(t *testing.T) {
	for _, b := range builtinProfiles() {
		p, err := profile.Load(b.data)
		if err != nil {
			t.Fatalf("%s: %v", b.id, err)
		}
		// A PV string's voltage and current that the power is computed
		// from were there before: they count as power readings.
		feedsPower := map[string]bool{}
		for _, d := range p.Derived {
			for _, n := range append(append([]string{}, d.SumOf...), d.ProductOf...) {
				feedsPower[n] = true
			}
		}
		seen := map[string]bool{}
		lastCore, firstVA := -1, len(p.Registers)
		for i, r := range p.Registers {
			if seen[r.Name] {
				t.Errorf("%s: reading %s twice", b.id, r.Name)
			}
			seen[r.Name] = true
			if !addedReadings[r.Name] || feedsPower[r.Name] {
				lastCore = i
				continue
			}
			if i < firstVA {
				firstVA = i
			}
			want := "A"
			switch {
			case strings.HasSuffix(r.Name, "_p_v") || strings.Contains(r.Name, "voltage"):
				want = "V"
			case r.Name == "grid_frequency":
				want = "Hz"
			}
			if r.Unit != want {
				t.Errorf("%s: %s unit %q, want %q", b.id, r.Name, r.Unit, want)
			}
		}
		// deye_sg04lp3 and renac_esc read grid/battery voltage among their
		// first registers since before; only the ones added later must trail.
		if b.id != "deye_sg04lp3" && b.id != "renac_esc" && firstVA < lastCore {
			t.Errorf("%s: voltage/current register before a power one (index %d < %d)", b.id, firstVA, lastCore)
		}
		for _, d := range p.Derived {
			if seen[d.Name] {
				t.Errorf("%s: derived %s also a register", b.id, d.Name)
			}
			seen[d.Name] = true
		}
		if !seen["grid_voltage"] {
			t.Errorf("%s: no grid_voltage", b.id)
		}
		if !seen["grid_frequency"] {
			t.Errorf("%s: no grid_frequency", b.id)
		}
	}
}
