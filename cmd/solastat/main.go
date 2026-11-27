package main

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"math"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"

	"solastat/internal/eventlog"
	"solastat/internal/history"
	"solastat/internal/modbus"
	"solastat/internal/mqttclient"
	"solastat/internal/pluginhub"
	"solastat/internal/profile"
	"solastat/internal/setupmode"
	"solastat/internal/solar"
	"solastat/internal/webgz"

	"github.com/pedronsg/solastat-template/pkg/pluginapi"

	apphub "github.com/OrbitOS-org/orbit-os-sdk-go/v26/api/app_hub_service/v26"
	ethsvc "github.com/OrbitOS-org/orbit-os-sdk-go/v26/api/ethernet_service/v26"
	"github.com/OrbitOS-org/orbit-os-sdk-go/v26/client"
	"github.com/OrbitOS-org/orbit-os-sdk-go/v26/logger"
	"github.com/OrbitOS-org/orbit-os-sdk-go/v26/metadata"

	"solastat/ext"
	"solastat/internal/extapi"
	"solastat/internal/fsutil"
)

// compiledPlugin is a compiled-in plugin, from the app's extension
// (solastat/ext: none in the default one).
type compiledPlugin = extapi.Plugin

// inverterRestartQuiet is how long alerts are held back after a restart
// from the dashboard: the inverter's start-up, while it reports a 0%
// battery and no meter (a Swatten's came 9 and 14 s after the restart).
const inverterRestartQuiet = 20 * time.Second

// inverterRestarter restarts the inverter (the dashboard's button): the
// plugins' inverter control, nil without them.
type inverterRestarter = extapi.InverterRestarter

// modbusAccess is the inverter's registers, for a plugin that needs them.
type modbusAccess = extapi.ModbusAccess

// toPluginReadings converts the core's own solar.Data into the shared
// pluginapi.Reading shape sent to every compiled-in plugin.
func toPluginReadings(d solar.Data) map[string]pluginapi.Reading {
	out := make(map[string]pluginapi.Reading, len(d.Readings))
	for name, r := range d.Readings {
		out[name] = pluginapi.Reading{Label: r.Label, Value: r.Value, Unit: r.Unit}
	}
	return out
}

// toPluginFaults converts the poller's faults to the plugin API's.
func toPluginFaults(fs []solar.ActiveFault) []pluginapi.Fault {
	out := make([]pluginapi.Fault, len(fs))
	for i, f := range fs {
		out[i] = pluginapi.Fault{ID: f.ID, Code: f.Code, Name: f.Name, Group: f.Group, Severity: f.Severity}
	}
	return out
}

// toPluginFaultReport is one poll cycle's faults for the plugins: unknown
// when the cycle couldn't tell (nil).
func toPluginFaultReport(f *solar.Fault) pluginapi.FaultReport {
	if f == nil {
		return pluginapi.FaultReport{Active: []pluginapi.Fault{}}
	}
	return pluginapi.FaultReport{Known: true, Active: toPluginFaults(f.Faults)}
}

const logTag = "main"

// Setup button and user LED of a board whose GPIO lines the app drives
// itself (extapi.Unit's GPIOSetup); on a board with a HAL they go through it
// instead (extapi.Board's UserIO), which holds the lines. The button reads LOW while pressed (pull-up on the PCB), and the LED lights
// with the pin LOW (wired from 3.3 V to the pin). The next PCB revision
// swaps them (LED on GPIO18 for hardware PWM, button on GPIO23): change only
// these lines then, including the LED polarity if it changes.
var (
	hubButtonPin    = &client.GpioPin{ChipNumber: 0, Number: 18}
	hubLedPin       = &client.GpioPin{ChipNumber: 0, Number: 23}
	hubLedActiveLow = true
)

// eventLogCapacity bounds the dashboard's in-RAM activity log — oldest
// entries fall off once full, nothing is persisted across a restart.
const eventLogCapacity = 500

//go:embed metadata.json
var metadataJSON []byte

// builtinProfilesFS holds the built-in inverter profiles, one
// orb/data/inverters/<id>.json each (see builtinProfileIDs).
//
//go:embed orb/data/inverters/*.json
var builtinProfilesFS embed.FS

//go:embed web/index.html
var dashboardHTML []byte

//go:embed web/settings.html
var settingsHTML []byte

//go:embed web/wizard.html
var wizardHTML []byte

//go:embed web/i18n.js
var i18nJS []byte

//go:embed web/nav.js
var navJS []byte

//go:embed orb/icon.svg
var iconSVG []byte

var appManifest = metadata.MustParseAppManifestJSON(metadataJSON)

// ── SSE broker ───────────────────────────────────────────────────────────────

type broker struct {
	mu      sync.Mutex
	clients map[chan string]struct{}
}

func newBroker() *broker { return &broker{clients: make(map[chan string]struct{})} }

func (b *broker) subscribe() chan string {
	ch := make(chan string, 8)
	b.mu.Lock()
	b.clients[ch] = struct{}{}
	b.mu.Unlock()
	return ch
}

func (b *broker) unsubscribe(ch chan string) {
	b.mu.Lock()
	delete(b.clients, ch)
	b.mu.Unlock()
}

func (b *broker) broadcast(msg string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for ch := range b.clients {
		select {
		case ch <- msg:
		default: // slow client — skip
		}
	}
}

// ── main ──────────────────────────────────────────────────────────────────────

func main() {
	host := flag.String("host", "192.168.5.226", "OrbitOS device IP address")
	uartPort := flag.String("uart", "", "UART port override (auto-detected from device if empty)")
	profileID := flag.String("profile", "swatten", "Inverter profile ID")
	dataDirFlag := flag.String("data-dir", "", "Directory for the app's data: settings, history, license keys, plugin files, and the inverter profiles in its inverters/ folder")
	httpPort := flag.Int("http-port", httpPortMin, fmt.Sprintf("HTTP dashboard port (kept within %d-%d)", httpPortMin, httpPortMax))
	devMode := flag.Bool("dev", false, "enable developer UI (Profile Manager)")
	flag.Parse()

	meta := metadata.Build(appManifest)
	logger.Init(meta.Name, "INFO", true)
	logger.Infof(logTag, "Starting %s", meta.Name)
	appManifest.PrintInfo()

	dataDir, err := resolveDataDir(*dataDirFlag)
	if err != nil {
		logger.Fatalf(logTag, "resolve data dir: %v", err)
		os.Exit(1)
	}

	// Inverter profiles, one JSON file each, have a folder of their own so
	// nothing else in the data dir is ever taken for one (or overwritten or
	// deleted as one).
	inverterDir := filepath.Join(dataDir, invertersDirName)
	if err := ensureDefaultProfiles(inverterDir); err != nil {
		logger.Fatalf(logTag, "prepare profiles dir: %v", err)
		os.Exit(1)
	}

	// The app's own settings (active profile, UART port, timezone, setup
	// done, notifications, MQTT) live in settings.json — see settings.go.
	settings := openSettings(dataDir)
	setupDone := func() bool { return settings.Get().SetupDone }

	// sysZone is the device's own time zone once the app's extension has read it
	// from a system with one (API 26.2), right after connecting.
	var sysZone systemZone
	getTimezone := func() string {
		return appTimezone(&sysZone, settings.Get().Timezone)
	}
	// appLocation is the app's timezone (appTimezone: the system's, or the
	// one set in SolaStat), which decides where a "day" starts everywhere
	// (dashboard history and totals, MQTT, the plugins, the LCD).
	appLocation := func() *time.Location {
		if l, err := time.LoadLocation(getTimezone()); err == nil {
			return l
		}
		return time.Local
	}

	getNotifCfg := func() NotificationsConfig { return settings.Get().Notifications }

	// historySeries is what the history records: each series and the live
	// reading it averages per minute. To record another reading, add a line
	// here (history.Open adds its column to existing databases). A future
	// Settings choice of which readings to keep would filter this list.
	historySeries := []struct {
		series  history.Series
		reading string
	}{
		{history.SeriesSolar, "pv_total_power"},
		{history.SeriesLoad, "total_consumption_power"},
		{history.SeriesGrid, "measured_power"},
		{history.SeriesBattery, "battery_power"},
		{history.SeriesBatterySoc, "battery_level"},
		// Voltages and currents: kept when the profile reads them (the
		// PV string names are the ones the ha-solarman profiles use).
		{history.SeriesGridV, "grid_voltage"},
		{history.SeriesGridA, "grid_current"},
		{history.SeriesGridVL2, "grid_voltage_l2"},
		{history.SeriesGridAL2, "grid_current_l2"},
		{history.SeriesGridVL3, "grid_voltage_l3"},
		{history.SeriesGridAL3, "grid_current_l3"},
		{history.SeriesGridF, "grid_frequency"},
		{history.SeriesPV1V, "pv1_total_power_p_v"},
		{history.SeriesPV1A, "pv1_total_power_p_i"},
		{history.SeriesPV2V, "pv2_total_power_p_v"},
		{history.SeriesPV2A, "pv2_total_power_p_i"},
		{history.SeriesPV3V, "pv3_total_power_p_v"},
		{history.SeriesPV3A, "pv3_total_power_p_i"},
		{history.SeriesPV4V, "pv4_total_power_p_v"},
		{history.SeriesPV4A, "pv4_total_power_p_i"},
		{history.SeriesBatteryV, "battery_voltage"},
		{history.SeriesBatteryA, "battery_current"},
	}

	// One history for the installation, whatever inverter profile reads it:
	// profiles all report the same readings with the same sign conventions,
	// so switching profile (a wrong pick, RS485 to Solarman, an edited copy,
	// a replaced inverter) keeps the history going.
	historyDBPath := filepath.Join(dataDir, "history.db")

	mqttClient := mqttclient.New(settings.Get().MQTT)
	mqttClient.Start()
	defer mqttClient.Stop()

	// App-level context cancelled on SIGTERM/SIGINT.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	// ── Load initial inverter profile ─────────────────────────────────────────
	// If --profile was not explicitly passed, restore the last used profile.
	profileFlagSet := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == "profile" {
			profileFlagSet = true
		}
	})
	if !profileFlagSet {
		if saved := settings.Get().ActiveProfile; saved != "" {
			*profileID = saved
		}
	}

	initialProf, err := loadProfile(*profileID, inverterDir)
	if err != nil {
		logger.Fatalf(logTag, "load profile %q: %v", *profileID, err)
		os.Exit(1)
	}
	logger.Infof(logTag, "Inverter profile: %s (slave 0x%02x, %d registers, poll %dms)",
		initialProf.Name, initialProf.Modbus.SlaveAddress, len(initialProf.Registers), initialProf.Modbus.PollIntervalMs)

	// ── History store (opened once, when there is data to record) ─────────────
	var historyMu sync.RWMutex
	var historyStore *history.Store

	openHistoryStore := func() *history.Store {
		path := historyDBPath
		series := make([]history.Series, len(historySeries))
		for i, h := range historySeries {
			series[i] = h.series
		}
		hs, hsErr := history.Open(path, series)
		if hsErr != nil {
			logger.Errorf(logTag, "open history db %s: %v — history disabled", path, hsErr)
			return nil
		}
		return hs
	}
	getHistoryStore := func() *history.Store {
		historyMu.RLock()
		defer historyMu.RUnlock()
		return historyStore
	}
	setHistoryStore := func(hs *history.Store) {
		historyMu.Lock()
		old := historyStore
		historyStore = hs
		historyMu.Unlock()
		if old != nil {
			old.Close()
		}
	}

	// Before the wizard is done nothing is connected, so there is nothing to
	// record yet: /api/setup/connect opens the history then, via switchProfile.
	if setupDone() {
		setHistoryStore(openHistoryStore())
	}
	defer func() { setHistoryStore(nil) }()

	// ── Connect to OrbitOS device (retry until context cancelled) ────────────
	var c *client.Client
	for {
		var connErr error
		c, connErr = client.NewClientAuto(*host)
		if connErr == nil {
			break
		}
		logger.Errorf(logTag, "connect to device: %v — retrying in 5s", connErr)
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Second):
		}
	}
	defer c.Close()

	// The device's system, as the app's extension reads it (solastat/ext).
	device := ext.Device(c)

	// The system's own time zone, where it has one: before anything goes by
	// the app's.
	device.WatchTimezone(ctx, &sysZone)

	// ── Board hardware (the app's extension) ─────────────────────────────────
	// What the extension adds of a board (a screen, relays, its LED and
	// button); nothing in the default one. onData hands it every poll.
	board := ext.StartBoard(ctx, extapi.Host{
		Client:   c,
		Version:  appManifest.Version,
		DataDir:  dataDir,
		Language: func() string { return settings.Get().Language },
		Location: appLocation,
		MonthPeriod: func(t time.Time) (time.Time, time.Time) {
			return monthPeriod(t, settings.Get().MonthStartDay, appLocation())
		},
		History: getHistoryStore,
	})

	hwModel, _ := c.SystemManager.GetHardwareModel()
	logger.Infof(logTag, "Device HW model: %s", hwModel)

	boardModel, _ := c.SystemManager.GetBoardModel()
	logger.Infof(logTag, "Device board model: %s", boardModel)
	// unit: what this unit is, as the app's extension knows it.
	unit := device.Unit(boardModel)
	logger.Infof(logTag, "Edition: %s", edition(unit))

	// ── Device serial (closest identity the SDK exposes) ─────────────────────
	deviceSerial, serialSource := resolveDeviceSerial(c)
	logger.Infof(logTag, "Device serial: %s (from %s)", deviceSerial, serialSource)

	// ── Resolve UART port ─────────────────────────────────────────────────────
	// Resolution happens in the background (see the retry goroutine below) so a
	// device with no UART ports enumerated yet (or none at all) never crashes
	// the app — it keeps checking every 2s until a port shows up.
	portOverride := *uartPort
	if port := unit.UARTPort; port != "" {
		portOverride = port
		logger.Infof(logTag, "Board with a fixed UART port: %s", port)
	} else if portOverride == "" {
		if saved := settings.Get().UARTPort; saved != "" {
			logger.Infof(logTag, "Using saved UART port: %s", saved)
			portOverride = saved
		}
	}

	// ── For RS485 on mini-UART: may need RTS/CTS control ────────────────────
	// Try to enable hardware flow control if available (some RS485 modules need this)
	logger.Infof(logTag, "Note: If using RS485, ensure RTS/CTS pins are connected for half-duplex control")

	// ── Open Modbus / UART (retry until context cancelled) ───────────────────
	// ── SSE broker + data store ───────────────────────────────────────────────
	hub := newBroker()
	var latestMu sync.RWMutex
	latest := solar.Data{Readings: map[string]solar.Reading{}}
	var comm commHealth // debounced "inverter answering", guarded by latestMu
	var stateMu sync.RWMutex
	activeProfileID := *profileID
	activeProfile := initialProf
	activePort := portOverride
	var mb modbus.Access
	var pollCancel context.CancelFunc

	// ── Plugin key store: relays license keys to compiled-in plugins (relay,
	// smartspot, ...) — each verifies its own key, the core just stores
	// what's pasted into Settings. The plugins themselves come from the
	// app's extension, below (none in the default one) ───────────────
	pluginKeysPath := filepath.Join(dataDir, "license_keys.json")
	pluginStore := pluginhub.New(deviceSerial, pluginKeysPath)

	registerPlugin := func(p compiledPlugin) {
		if p == nil {
			return
		}
		pluginStore.RegisterInfo(func() pluginhub.Info {
			i := p.Info()
			return pluginhub.Info{ID: i.ID, Version: i.Version, Authorized: i.Authorized,
				Name: i.Name, Description: i.Description, Icon: i.Icon, Category: i.Category, Path: i.Path}
		})
	}

	// ── Activity log: a bounded, in-RAM ring buffer for the dashboard's
	// Logs card — inverter status (reported by relay, which already reads
	// it to decide when to act), relay on/off, smartspot write attempts.
	// Deliberately not persisted: capped at eventLogCapacity entries,
	// oldest falls off, starts empty on every restart. ───────────────────
	eventLog := eventlog.New(eventLogCapacity)

	// allPlugins is whatever the compiled-in plugins report, opaque to this file
	// — no plugin is named here, adding or removing one on the other side
	// in the extension needs no change in solastat. Declared before
	// pluginLogger/collectNotifTypes below (both close over it by
	// reference) since ext.Plugins itself needs pluginLogger as an
	// argument — the closures are never actually invoked until a poll/tick
	// happens, well after ext.Plugins has returned and assigned it.
	var allPlugins []compiledPlugin

	// collectNotifTypes gathers every currently-authorized compiled-in
	// plugin's declared notification types (see
	// pluginapi.NotificationTypeProvider), namespaced "<plugin ID>:<type
	// ID>" so two plugins can never collide. The core never hardcodes which
	// plugins exist — this is how a new plugin's notification checkboxes
	// appear without touching this file. A plugin still compiled in but
	// unlicensed (no valid key pasted in Settings) is skipped, same as it
	// already is on the Plugins card's own listing (pluginStore.Status()).
	collectNotifTypes := func() []notifPluginType {
		var out []notifPluginType
		for _, p := range allPlugins {
			provider, ok := p.(pluginapi.NotificationTypeProvider)
			if !ok {
				continue
			}
			info := p.Info()
			if !info.Authorized {
				continue
			}
			id := info.ID
			for _, t := range provider.NotificationTypes() {
				out = append(out, notifPluginType{Key: id + ":" + t.ID, Label: t.Label, Labels: t.Labels, DefaultEnabled: t.DefaultEnabled})
			}
		}
		return out
	}

	// sendNotification is a no-op unless the first-run wizard has been
	// completed (see settings.SetupDone — a default profile mid-wizard shouldn't
	// be alerting anyone) and both the master switch and the specific
	// event's own checkbox are on. Runs the actual SDK call in a goroutine
	// — this is called from the poll cycle and the plugin event path,
	// neither of which should block on a network round-trip to the
	// Settings app.
	sendNotification := func(enabled bool, body string) {
		if !setupDone() {
			return // first-run wizard not completed yet — nothing to notify about
		}
		cfg := getNotifCfg()
		if !cfg.Enabled || !enabled {
			return
		}
		go func() {
			if _, _, err := c.MobileNotificationManager.Send(body, nil); err != nil {
				logger.Warnf(logTag, "send notification: %v", err)
			}
		}()
	}

	pluginLogger := func(source string) func(pluginapi.LogEvent) {
		return func(e pluginapi.LogEvent) {
			eventLog.Add(eventlog.Entry{Ts: time.Now(), Source: source, Kind: e.Kind, On: e.On, Code: e.Code, Text: e.Text, OK: e.OK})

			if e.NotifyType == "" || e.Text == "" {
				return
			}
			key := source + ":" + e.NotifyType
			enabled := true // fails open if the plugin didn't declare this key (shouldn't happen)
			for _, nt := range collectNotifTypes() {
				if nt.Key == key {
					enabled = nt.DefaultEnabled
					break
				}
			}
			cfg := getNotifCfg()
			if v, ok := cfg.PluginTypes[key]; ok {
				enabled = v
			}
			sendNotification(enabled, e.Text)
		}
	}

	// smartspot's Modbus register access is direct, in-process access to
	// the one *modbus.Client this core owns — never RPC. Reading
	// activeProfile fresh on every call (not just at construction time)
	// means a plugin needing Modbus access always sees the current
	// inverter profile's slave address, even after the user switches
	// profiles in Settings. (Passed to ext.Plugins whether
	// or not any compiled-in plugin actually uses it.)
	smartSpotGetModbus := func() (modbusAccess, byte, bool) {
		stateMu.RLock()
		defer stateMu.RUnlock()
		if mb == nil {
			return nil, 0, false
		}
		return mb, activeProfile.Modbus.SlaveAddress, true
	}

	// The plugins' inverter control (pluginapi.InverterControl) picks its
	// driver for the active inverter from this, instead of a separate,
	// manually-chosen setting of its own. Reading activeProfileID
	// fresh on every call, same as smartSpotGetModbus above, means a
	// profile switch in Settings takes effect immediately. A copy of a
	// built-in profile under an ID of its own is driven as its base.
	getActiveProfileID := func() string {
		stateMu.RLock()
		defer stateMu.RUnlock()
		return controlProfileID(activeProfileID, activeProfile)
	}

	// getFaultCatalog lists every fault the active profile can report, with
	// the IDs each poll reports them by (solar.FaultCatalog), for a plugin's
	// settings (the relay's "Inverter error" mode).
	getFaultCatalog := func() pluginapi.FaultCatalog {
		stateMu.RLock()
		p := activeProfile
		stateMu.RUnlock()
		if p == nil {
			return pluginapi.FaultCatalog{Kind: pluginapi.FaultsNone}
		}
		kind, faults := solar.FaultCatalog(p)
		return pluginapi.FaultCatalog{Kind: kind, Faults: toPluginFaults(faults)}
	}

	// The relay plugin drives a board's relays through its HAL (the
	// extension's, nil without it), other boards' through the SDK's GPIO.
	var inverterRestart inverterRestarter
	allPlugins, inverterRestart = ext.Plugins(extapi.PluginHost{
		Client:             c,
		DataDir:            dataDir,
		DeviceSerial:       deviceSerial,
		GetKeys:            pluginStore.Keys,
		GetModbus:          smartSpotGetModbus,
		GetTimezone:        getTimezone,
		GetActiveProfileID: getActiveProfileID,
		GetFaultCatalog:    getFaultCatalog,
		Product:            unit.Product,
		HALRelays:          board.Relays(),
		LogEvent:           pluginLogger,
	})
	for _, p := range allPlugins {
		registerPlugin(p)
		if closer, ok := p.(interface{ Close() error }); ok {
			defer closer.Close()
		}
		// A board's screen shows the relays of the plugin that drives them,
		// and the prices of the one that shares them.
		if rs, ok := p.(pluginapi.RelayStateProvider); ok {
			board.RelayStates(rs.RelayStates)
		}
		if pp, ok := p.(pluginapi.PriceProvider); ok {
			board.Prices(pp.Prices)
		}
		if fp, ok := p.(pluginapi.ForecastProvider); ok {
			board.Forecast(fp.Forecast)
		}
	}

	// notifEdge tracks transitions across poll cycles so each core event
	// notifies once when it starts, not every ~5s while the condition holds
	// (e.g. a battery sitting at 100% all afternoon, or a comms outage that
	// lasts several poll cycles).
	var notifEdge struct {
		mu             sync.Mutex
		batteryFull    bool
		batteryLow     bool
		hadCommError   bool
		hadInverterErr bool
		faultIDs       map[string]bool // the inverter's faults active last cycle
		// quietUntil holds every alert back while an inverter restarted
		// from the dashboard starts again: it reports a 0% battery and
		// faults (no meter yet) that are only its start-up. Nothing is
		// tracked meanwhile, so what still holds afterwards is logged then.
		quietUntil time.Time
	}

	// logCoreEvent records a core-detected (non-plugin) notification-worthy
	// event into the same activity log the dashboard's Logs card already
	// shows plugin events in, then notifies if enabled is true — mirrors
	// pluginLogger's own "always log, conditionally notify" split, so the
	// Logs card reflects what actually happened independent of whether the
	// user has that particular notification type turned on.
	logCoreEvent := func(kind string, code int, text string, enabled bool) {
		eventLog.Add(eventlog.Entry{Ts: time.Now(), Source: "core", Kind: kind, Code: code, Text: text})
		sendNotification(enabled, text)
	}
	// logCoreEventArgs is logCoreEvent with Args for the page's translation.
	logCoreEventArgs := func(kind string, code int, text string, args map[string]string, enabled bool) {
		eventLog.Add(eventlog.Entry{Ts: time.Now(), Source: "core", Kind: kind, Code: code, Text: text, Args: args})
		sendNotification(enabled, text)
	}

	// ── Setup button, user LED and Wi-Fi setup (AP) mode ─────────────────────
	// On a board whose button and LED are on the GPIO lines (unit.GPIOSetup),
	// or come through its screen (unit.ScreenSetup, the app's extension).
	// Started before anything that talks to the inverter, so the button keeps
	// working (and the device can always be put back on Wi-Fi) whatever state
	// the Modbus side is in. A board on Ethernet only opens its AP by itself
	// with no network at all (WiredUp). Other boards get a nil *Service, whose
	// HTTP handlers report setup mode as unavailable.
	var setup *setupmode.Service
	setupHAL := board.UserIO(unit.ScreenSetup)
	if unit.GPIOSetup || (unit.ScreenSetup && setupHAL != nil) {
		setup = setupmode.New(setupmode.Config{
			GPIO:             c.GpioManager,
			WiFi:             c.WiFiManager,
			HAL:              setupHAL,
			ButtonPin:        hubButtonPin,
			LedPin:           hubLedPin,
			LedActiveLow:     hubLedActiveLow,
			Serial:           deviceSerial,
			DeviceHost:       deviceHost(device),
			JoinedMarkerPath: filepath.Join(dataDir, "wifi_joined.cfg"),
			CommOK: func() bool {
				latestMu.RLock()
				defer latestMu.RUnlock()
				return comm.ok
			},
			OnEvent: func(kind, text string) {
				eventLog.Add(eventlog.Entry{Ts: time.Now(), Source: "core", Kind: kind, Text: text})
			},
			WiredUp: func() bool { return wiredUp(c) },
			OpenAP:  device.OpenSetupAP(),
		})
		board.Setup(setup.Setup)
		go setup.Run(ctx)
	}

	var (
		mqttTotals    history.Totals // day totals last sent over MQTT
		mqttTotalsAt  time.Time
		mqttTotalsDay time.Time      // the day mqttTotals are of
		mqttLifeBase  history.Totals // totals before today (for the lifetime sensors)
		mqttLifeDay   time.Time      // the day mqttLifeBase was computed for
	)
	onData := func(d solar.Data) {
		latestMu.Lock()
		latest = d
		comm.observe(d.Error, len(d.Readings))
		commOK, commDown, commErr := comm.ok, comm.down(), comm.lastErr
		latestMu.Unlock()

		readings := toPluginReadings(d)
		faults := toPluginFaultReport(d.Fault)
		for _, p := range allPlugins {
			p.OnReading(readings)
			if fo, ok := p.(pluginapi.FaultObserver); ok {
				fo.OnFaults(faults)
			}
		}

		func() {
			cfg := getNotifCfg()
			notifEdge.mu.Lock()
			defer notifEdge.mu.Unlock()
			if time.Now().Before(notifEdge.quietUntil) {
				return
			}

			if level := readingValue(d, "battery_level"); !math.IsNaN(level) {
				if level >= 100 {
					if !notifEdge.batteryFull {
						notifEdge.batteryFull = true
						logCoreEvent("battery_full", 0, "Battery charged to 100%", cfg.BatteryFull)
					}
				} else {
					notifEdge.batteryFull = false
				}
				if level <= cfg.BatteryLowThreshold {
					if !notifEdge.batteryLow {
						notifEdge.batteryLow = true
						logCoreEvent("battery_low", int(level), fmt.Sprintf("Battery low: %.0f%%", level), cfg.BatteryLow)
					}
				} else {
					notifEdge.batteryLow = false
				}
			}

			// Debounced by commHealth: only after commErrorAfterCycles failed
			// cycles in a row, so one dropped request doesn't notify.
			if commDown && !notifEdge.hadCommError {
				notifEdge.hadCommError = true
				logCoreEvent("comm_error", 0, fmt.Sprintf("Communication error: %s", commErr), cfg.CommError)
			} else if commOK && notifEdge.hadCommError {
				notifEdge.hadCommError = false
				logCoreEvent("comm_ok", 0, "Device recovered — communication restored", cfg.DeviceRecovered)
			}

			if f := d.Fault; f != nil {
				fresh, now := newRegisterFaults(notifEdge.faultIDs, f.Faults)
				notifEdge.faultIDs = now
				if f.Active {
					switch {
					case len(fresh) > 0:
						// Named by the profile's fault registers: logged for
						// every new one, even while another is still active.
						notifEdge.hadInverterErr = true
						names := faultNames(fresh)
						logCoreEventArgs("inverter_fault_named", 0, "Inverter fault: "+names, map[string]string{"names": names}, cfg.InverterFault)
					case !notifEdge.hadInverterErr:
						notifEdge.hadInverterErr = true
						// A state (Deye's run state) isn't an error code: its
						// own kind, so the log doesn't show "error 4".
						kind, text := "inverter_fault", fmt.Sprintf("Inverter fault: code %d", f.Code)
						if f.Kind == "state" {
							kind, text = "inverter_fault_state", fmt.Sprintf("Inverter fault: state %d", f.Code)
						}
						// A status code the profile names (Swatten's table): "code 612 · Battery Abnormality".
						var args map[string]string
						if len(f.Faults) > 0 && f.Faults[0].Source == solar.FaultFromStatus && f.Faults[0].Name != "" {
							args = map[string]string{"name": f.Faults[0].Name}
							text += " · " + f.Faults[0].Name
						}
						logCoreEventArgs(kind, f.Code, text, args, cfg.InverterFault)
					}
				} else {
					notifEdge.hadInverterErr = false
				}
			}
		}()

		hs := getHistoryStore()
		if hs != nil && len(d.Readings) > 0 {
			sample := make(history.Sample, len(historySeries))
			for _, h := range historySeries {
				sample[h.series.Column] = readingValue(d, h.reading)
			}
			hs.AddSample(d.Ts, sample)
		}

		b, _ := json.Marshal(d)
		hub.broadcast(string(b))

		stateMu.RLock()
		inverter := ""
		if activeProfile != nil {
			inverter = activeProfile.Name
		}
		stateMu.RUnlock()
		board.Update(d, commOK, inverter)

		// MQTT: only when a broker is connected, and the day totals (a SQL sum
		// over today's rows) are refreshed once a minute, not on every reading.
		if mqttClient.Connected() {
			p := mqttclient.MakePayload(
				readingValue(d, "pv_total_power"),
				readingValue(d, "total_consumption_power"),
				readingValue(d, "measured_power"),
				readingValue(d, "battery_power"),
				readingValue(d, "battery_level"),
				readingValue(d, "battery_health"),
			)
			// Status and every raw reading of the profile, for HA.
			p.CommOK = commOK
			p.InverterFault = d.Fault != nil && d.Fault.Active
			for name, r := range d.Readings {
				p.AddReading(name, r.Label, r.Unit, r.Value)
			}
			if hs != nil {
				now := time.Now()
				if now.Sub(mqttTotalsAt) >= time.Minute {
					loc := appLocation()
					nowLoc := now.In(loc)
					dayStart := time.Date(nowLoc.Year(), nowLoc.Month(), nowLoc.Day(), 0, 0, 0, 0, loc)
					// Lifetime = everything before today (summed once a day)
					// + today's running totals.
					if !mqttLifeDay.Equal(dayStart) {
						if t, err := hs.QueryTotals(context.Background(), time.Unix(0, 0), dayStart); err == nil {
							mqttLifeBase, mqttLifeDay = t, dayStart
						}
					}
					if t, err := hs.QueryTotals(context.Background(), dayStart, now); err == nil {
						mqttTotals, mqttTotalsAt, mqttTotalsDay = t, now, dayStart
					}
				}
				if !mqttTotalsAt.IsZero() {
					t := mqttTotals
					mqttclient.AddTotals(&p, t.SolarKWh, t.LoadKWh, t.ImportKWh, t.ExportKWh, t.BChargedKWh, t.BDischargedKWh)
					// Only with the base of the same day: an old base (its
					// sum failed past midnight) under today's totals would
					// go down, and HA would take that for a meter reset.
					// None sent is unavailable in HA, then right again.
					if !mqttLifeDay.IsZero() && mqttLifeDay.Equal(mqttTotalsDay) {
						b := mqttLifeBase
						mqttclient.AddLifetimeTotals(&p, b.SolarKWh+t.SolarKWh, b.LoadKWh+t.LoadKWh,
							b.ImportKWh+t.ImportKWh, b.ExportKWh+t.ExportKWh,
							b.BChargedKWh+t.BChargedKWh, b.BDischargedKWh+t.BDischargedKWh)
					}
				}
			}
			mqttClient.Publish(p)
		}

		logger.Debugf(logTag, "poll complete — %d readings, err=%q", len(d.Readings), d.Error)
	}

	// ── Start/replace active profile runtime ─────────────────────────────────
	// switchMu serializes every change of connection (the startup retry loop,
	// the wizard and Settings can all ask for one at the same time), so two
	// callers never both open the port and leave two pollers running.
	var switchMu sync.Mutex

	// stopConnection closes the inverter connection (if any) and stops
	// polling. Callers hold switchMu.
	stopConnection := func() {
		stateMu.Lock()
		oldCancel := pollCancel
		oldMB := mb
		pollCancel = nil
		mb = nil
		stateMu.Unlock()
		if oldCancel != nil {
			oldCancel()
		}
		if oldMB != nil {
			oldMB.Stop()
		}
	}

	switchProfile := func(nextID string) error {
		switchMu.Lock()
		defer switchMu.Unlock()
		nextProf, err := loadProfile(nextID, inverterDir)
		if err != nil {
			return err
		}
		stateMu.RLock()
		port := activePort
		stateMu.RUnlock()

		// Important: stop previous UART session before opening the next one on
		// the same port. Otherwise the old Close() can invalidate the new session.
		// If opening the next one fails, no stopped client is left behind for
		// the plugins to use.
		stopConnection()

		var nextMB modbus.Access
		switch nextProf.Modbus.Connection {
		case profile.ConnSolarman:
			host := strings.TrimSpace(nextProf.Modbus.TCPHost)
			if host == "" {
				return fmt.Errorf("profile %s uses a Solarman datalogger but its IP address is empty", nextID)
			}
			sc := modbus.NewSolarmanClient(host, nextProf.Modbus.TCPSerial)
			if nextProf.Modbus.TCPSerial == 0 {
				// No serial: the client asks the logger for it, and it's kept
				// in the profile, unless the profile changed meanwhile.
				sc.OnSerialLearned(func(serial uint32) {
					p, err := loadProfile(nextID, inverterDir)
					if err != nil {
						logger.Warnf(logTag, "keep the Solarman serial %d: %v", serial, err)
						return
					}
					if p.Modbus.Connection != profile.ConnSolarman || strings.TrimSpace(p.Modbus.TCPHost) != host || p.Modbus.TCPSerial != 0 {
						return
					}
					p.Modbus.TCPSerial = serial
					if err := saveProfile(inverterDir, nextID, p); err != nil {
						logger.Warnf(logTag, "keep the Solarman serial %d: %v", serial, err)
						return
					}
					logger.Infof(logTag, "profile %s: Solarman serial %d, told by the logger, saved", nextID, serial)
				})
			}
			nextMB = sc
			logger.Infof(logTag, "Active profile: %s (%s) via Solarman TCP %s (serial %d)",
				nextID, nextProf.Name, host, nextProf.Modbus.TCPSerial)
		case profile.ConnModbusTCP:
			host := strings.TrimSpace(nextProf.Modbus.TCPHost)
			if host == "" {
				return fmt.Errorf("profile %s uses Modbus TCP but the inverter's IP address is empty", nextID)
			}
			port := nextProf.Modbus.ModbusTCPPort()
			nextMB = modbus.NewTCPClient(host, port)
			logger.Infof(logTag, "Active profile: %s (%s) via Modbus TCP %s:%d (unit %d)",
				nextID, nextProf.Name, host, port, nextProf.Modbus.SlaveAddress)
		case profile.ConnRTUOverTCP:
			host := strings.TrimSpace(nextProf.Modbus.TCPHost)
			if host == "" {
				return fmt.Errorf("profile %s uses Modbus RTU over TCP but the gateway's IP address is empty", nextID)
			}
			port := nextProf.Modbus.RTUOverTCPPort()
			nextMB = modbus.NewRTUOverTCPClient(host, port)
			logger.Infof(logTag, "Active profile: %s (%s) via Modbus RTU over TCP %s:%d (slave %d)",
				nextID, nextProf.Name, host, port, nextProf.Modbus.SlaveAddress)
		default:
			rtuMB := modbus.NewClient(c.UartManager, port)
			if err := rtuMB.Start(ctx, nextProf.UartConfig(port)); err != nil {
				return fmt.Errorf("start modbus on %s: %w", port, err)
			}
			nextMB = rtuMB
			logger.Infof(logTag, "Active profile: %s (%s) on %s @ %d baud",
				nextID, nextProf.Name, port, nextProf.Uart.BaudRate)
		}

		pollCtx, cancel := context.WithCancel(ctx)
		stateMu.Lock()
		prevID, prevProf := activeProfileID, activeProfile
		pollCancel = cancel
		mb = nextMB
		activeProfileID = nextID
		activeProfile = nextProf
		stateMu.Unlock()

		// The history is the installation's, not the profile's: a switch is
		// recorded in the activity log, so it can be told apart later.
		if setupDone() && prevID != "" && prevID != nextID && prevProf != nil {
			eventLog.Add(eventlog.Entry{Ts: time.Now(), Source: "core", Kind: "profile_changed",
				Text: fmt.Sprintf("Inverter profile changed from %s to %s", prevProf.Name, nextProf.Name),
				Args: map[string]string{"from": prevProf.Name, "to": nextProf.Name}})
		}

		// smartSpotGetModbus reads activeProfile fresh on every call, so a
		// plugin needing Modbus access sees the new slave address on its
		// very next call — no separate notification needed.

		if getHistoryStore() == nil {
			setHistoryStore(openHistoryStore())
		}

		poller := solar.NewPoller(nextMB, nextProf, onData)
		go poller.Run(pollCtx)

		return nil
	}

	// applyProfile makes id the active profile. Once setup is done that means
	// (re)connecting to the inverter right away (switchProfile). While the
	// setup wizard is still running it only records the choice: the port is
	// not opened until the user has confirmed how the inverter is connected
	// (wizard step 2 → POST /api/setup/connect).
	applyProfile := func(id string) error {
		if setupDone() {
			return switchProfile(id)
		}
		switchMu.Lock()
		defer switchMu.Unlock()
		nextProf, err := loadProfile(id, inverterDir)
		if err != nil {
			return err
		}
		stopConnection()
		stateMu.Lock()
		activeProfileID = id
		activeProfile = nextProf
		stateMu.Unlock()
		logger.Infof(logTag, "Selected profile %s (%s): waiting for the wizard to confirm the connection", id, nextProf.Name)
		return nil
	}

	// Do not block app startup on UART port resolution or Modbus/profile
	// bring-up. Keep retrying in background: if the device has no UART ports
	// yet (or none at all), just keep checking every 2s instead of failing —
	// unless the initial profile connects over TCP, which needs no UART
	// port at all.
	//
	// Until the setup wizard has been completed, nothing is opened or polled:
	// the inverter hasn't been chosen yet, so talking to the default profile
	// would only fill the log with timeouts. The port is still resolved (the
	// wizard shows it), and the wizard's own profile/port step starts the
	// connection through switchProfile when the user picks the inverter.
	// startProfile is one startup attempt. It connects the profile that is
	// active now (the user may have picked another one while this loop was
	// retrying) and does nothing if something else already connected.
	startProfile := func() error {
		stateMu.RLock()
		id, running := activeProfileID, mb != nil
		stateMu.RUnlock()
		if running {
			return nil
		}
		return switchProfile(id)
	}

	go func() {
		waitForWizard := !setupDone()
		if waitForWizard {
			logger.Infof(logTag, "setup not completed yet: not connecting to the inverter until the wizard picks it")
		}
		if initialProf.Modbus.IsNetwork() {
			if waitForWizard {
				return
			}
			for {
				if err := startProfile(); err == nil {
					return
				} else {
					logger.Errorf(logTag, "start inverter connection failed: %v — retrying in 5s", err)
				}
				select {
				case <-ctx.Done():
					return
				case <-time.After(5 * time.Second):
				}
			}
		}
		for {
			port, err := resolveUartPort(c.UartManager, portOverride)
			if err != nil {
				logger.Errorf(logTag, "resolve UART port: %v — retrying in 2s", err)
				select {
				case <-ctx.Done():
					return
				case <-time.After(2 * time.Second):
				}
				continue
			}

			stateMu.Lock()
			activePort = port
			stateMu.Unlock()
			logger.Infof(logTag, "Using UART port: %s", port)
			if waitForWizard {
				return
			}

			if err := startProfile(); err == nil {
				return
			} else {
				logger.Errorf(logTag, "start inverter connection failed: %v — retrying in 5s", err)
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(5 * time.Second):
			}
		}
	}()

	// Every running plugin gets ticked on the same fixed 1s cadence — see
	// pluginapi.Hooks.Tick's doc comment for why the core deliberately
	// never has a per-plugin interval to know about; a plugin needing a
	// slower cadence (e.g. smartspot's 30s decision cycle) rate-limits
	// itself internally.
	go func() {
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				for _, p := range allPlugins {
					p.Tick()
				}
			}
		}
	}()

	defer func() {
		stateMu.Lock()
		if pollCancel != nil {
			pollCancel()
		}
		if mb != nil {
			mb.Stop()
		}
		stateMu.Unlock()
	}()

	// ── HTTP server ───────────────────────────────────────────────────────────
	mux := http.NewServeMux()

	// Wrap all handlers with logging middleware
	logHandler := func(name string, h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			logger.Debugf(logTag, "HTTP %s %s", r.Method, r.RequestURI)
			h(w, r)
		}
	}

	// Register API endpoints FIRST - more specific patterns first
	mux.HandleFunc("/api/data", logHandler("api/data", func(w http.ResponseWriter, r *http.Request) {
		latestMu.RLock()
		d := latest
		latestMu.RUnlock()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(d)
	}))

	mux.HandleFunc("/api/profile", logHandler("api/profile", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			var req struct {
				ID string `json:"id"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.ID) == "" {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid body, expected {\"id\":\"...\"}"})
				return
			}
			newID := strings.TrimSpace(req.ID)
			if err := applyProfile(newID); err != nil {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
				return
			}
			if err := settings.Update(func(s *appSettings) { s.ActiveProfile = newID }); err != nil {
				logger.Warnf(logTag, "save active profile: %v", err)
			}
		}
		stateMu.RLock()
		p := activeProfile
		currentID := activeProfileID
		stateMu.RUnlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"id":      currentID,
			"profile": p,
		})
	}))

	mux.HandleFunc("/api/totals", logHandler("api/totals", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		hs := getHistoryStore()
		if hs == nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "history not available"})
			return
		}
		loc := appLocation()
		now := time.Now().In(loc)
		var anchor time.Time
		if dateParam := strings.TrimSpace(r.URL.Query().Get("date")); dateParam != "" {
			parsed, err := time.ParseInLocation("2006-01-02", dateParam, loc)
			if err != nil {
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": fmt.Sprintf("invalid date %q (expected YYYY-MM-DD)", dateParam)})
				return
			}
			anchor = parsed
		} else {
			anchor = now
		}
		var start, end time.Time
		switch r.URL.Query().Get("period") {
		case "day":
			start = time.Date(anchor.Year(), anchor.Month(), anchor.Day(), 0, 0, 0, 0, loc)
			end = start.AddDate(0, 0, 1)
		case "month":
			start, end = monthPeriod(anchor, settings.Get().MonthStartDay, loc)
		case "year":
			start = time.Date(anchor.Year(), 1, 1, 0, 0, 0, 0, loc)
			end = start.AddDate(1, 0, 0)
		default:
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid period (expected day|month|year)"})
			return
		}
		totals, err := hs.QueryTotals(r.Context(), start, end)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		_ = json.NewEncoder(w).Encode(totals)
	}))

	mux.HandleFunc("/api/config", logHandler("api/config", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"dev":        *devMode,
			"boardModel": boardModel,
			// product: the unit's product id, "" for none.
			"product": unit.Product,
			// hub: the board's setup button and LED are on the GPIO lines.
			"hub": unit.GPIOSetup,
			// fixedUARTPort: the UART port wired to the board's RS485, ""
			// when the user picks it. The wizard and Settings hide the port
			// picker when it is set.
			"fixedUARTPort": unit.UARTPort,
			// setupDone: the wizard shows the full nav bar only when it is
			// re-run after setup (the other pages aren't usable before).
			"setupDone": setupDone(),
		})
	}))

	mux.HandleFunc("/api/version", logHandler("api/version", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// edition: what the logo says next to SolaStat on every page (nav.js).
		_ = json.NewEncoder(w).Encode(map[string]string{"version": appManifest.Version, "edition": edition(unit)})
	}))

	mux.HandleFunc("/api/profiles", logHandler("api/profiles", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			items, err := listProfiles(inverterDir)
			if err != nil {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusInternalServerError)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
				return
			}
			stateMu.RLock()
			currentID := activeProfileID
			stateMu.RUnlock()
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"current":     currentID,
				"profilesDir": inverterDir,
				"profiles":    items,
				"canWrite":    dataDir != "",
				"builtinOnly": false,
			})
		case http.MethodPost:
			var req struct {
				ID      string          `json:"id"`
				Profile profile.Profile `json:"profile"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid profile JSON"})
				return
			}
			if err := saveProfile(inverterDir, req.ID, &req.Profile); err != nil {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
				return
			}
			// If the saved profile is currently active, reload it so the new
			// settings (e.g. poll_interval_ms) take effect immediately.
			savedID := strings.TrimSpace(req.ID)
			if savedID == "" {
				savedID = slugFromName(req.Profile.Name)
			}
			stateMu.RLock()
			currentID := activeProfileID
			stateMu.RUnlock()
			if savedID == currentID {
				if err := applyProfile(currentID); err != nil {
					logger.Warnf(logTag, "reload active profile after save: %v", err)
				}
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]bool{"success": true})
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))

	mux.HandleFunc("/api/profiles/", logHandler("api/profiles/id", func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.URL.Path, "/api/profiles/")
		if r.Method == http.MethodGet {
			p, err := loadProfile(id, inverterDir)
			if err != nil {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusNotFound)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(p)
			return
		}
		if r.Method != http.MethodDelete {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if err := deleteProfile(inverterDir, id); err != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]bool{"success": true})
	}))

	mux.HandleFunc("/api/ports", logHandler("api/ports", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			var req struct {
				Port string `json:"port"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Port) == "" {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid body, expected {\"port\":\"...\"}"})
				return
			}
			ports, err := c.UartManager.ListPorts()
			if err != nil {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusInternalServerError)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": fmt.Sprintf("list uart ports: %v", err)})
				return
			}
			nextPort := strings.TrimSpace(req.Port)
			ok := false
			for _, p := range ports {
				if p == nextPort {
					ok = true
					break
				}
			}
			if !ok {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": fmt.Sprintf("port %q not available", nextPort)})
				return
			}
			stateMu.Lock()
			activePort = nextPort
			currentID := activeProfileID
			stateMu.Unlock()
			if err := applyProfile(currentID); err != nil {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
				return
			}
			if err := settings.Update(func(s *appSettings) { s.UARTPort = nextPort }); err != nil {
				logger.Warnf(logTag, "save UART port: %v", err)
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]bool{"success": true})
			return
		}
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		logger.Infof(logTag, "api/ports: calling UartManager.ListPorts()")
		ports, err := c.UartManager.ListPorts()
		if err != nil {
			logger.Errorf(logTag, "api/ports: ListPorts() failed: %v", err)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			stateMu.RLock()
			currentPort := activePort
			stateMu.RUnlock()
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"error":   fmt.Sprintf("list uart ports: %v", err),
				"ports":   []string{},
				"current": currentPort,
			})
			return
		}
		stateMu.RLock()
		currentPort := activePort
		stateMu.RUnlock()
		logger.Infof(logTag, "api/ports: success - %v (current=%s)", ports, currentPort)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"ports":   ports,
			"current": currentPort,
		})
	}))

	mux.HandleFunc("/api/history", logHandler("api/history", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		hs := getHistoryStore()
		if hs == nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "history database is not available"})
			return
		}
		rangeKind := strings.TrimSpace(r.URL.Query().Get("range"))
		if rangeKind == "" {
			rangeKind = "day"
		}
		dateParam := strings.TrimSpace(r.URL.Query().Get("date"))
		loc := appLocation()
		var anchor time.Time
		if dateParam == "" {
			anchor = time.Now().In(loc)
		} else {
			parsed, err := time.ParseInLocation("2006-01-02", dateParam, loc)
			if err != nil {
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": fmt.Sprintf("invalid date %q (expected YYYY-MM-DD)", dateParam)})
				return
			}
			anchor = parsed
		}
		var (
			start, end time.Time
			gran       history.Granularity
		)
		switch rangeKind {
		case "day":
			start = time.Date(anchor.Year(), anchor.Month(), anchor.Day(), 0, 0, 0, 0, loc)
			end = start.AddDate(0, 0, 1)
			gran = history.GranMinute
		case "month":
			start, end = monthPeriod(anchor, settings.Get().MonthStartDay, loc)
			gran = history.GranHour
		case "year":
			start = time.Date(anchor.Year(), 1, 1, 0, 0, 0, 0, loc)
			end = start.AddDate(1, 0, 0)
			gran = history.GranDay
		default:
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": fmt.Sprintf("invalid range %q (expected day|month|year)", rangeKind)})
			return
		}
		buckets, err := hs.Query(r.Context(), start, end, gran, loc)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"range":       rangeKind,
			"granularity": string(gran),
			"date":        anchor.Format("2006-01-02"),
			"start":       start,
			"end":         end,
			"timezone":    loc.String(),
			"buckets":     buckets,
		})
	}))

	// GET /api/history/export.csv?from=YYYY-MM-DD&to=YYYY-MM-DD&format=standard|excel
	// or ?start=<unix s>&end=<unix s>&format=… (a time window, as the main
	// page's chart shows it, end excluded).
	// Streams the history for the local days [from, to] as CSV, as recorded
	// (one row per stored minute, see history.Store.WriteCSV). Errors before the first byte come back
	// as JSON so the Settings page can show them.
	mux.HandleFunc("/api/history/export.csv", logHandler("api/history/export", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		fail := func(status int, msg string) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
		}
		hs := getHistoryStore()
		if hs == nil {
			fail(http.StatusServiceUnavailable, "history database is not available")
			return
		}
		q := r.URL.Query()
		loc := appLocation()
		var from, end time.Time
		var name string
		if q.Get("start") != "" || q.Get("end") != "" {
			startS, errStart := strconv.ParseInt(strings.TrimSpace(q.Get("start")), 10, 64)
			endS, errEnd := strconv.ParseInt(strings.TrimSpace(q.Get("end")), 10, 64)
			if errStart != nil || errEnd != nil {
				fail(http.StatusBadRequest, "start and end must be unix times in seconds")
				return
			}
			from, end = time.Unix(startS, 0).In(loc), time.Unix(endS, 0).In(loc)
			if !end.After(from) {
				fail(http.StatusBadRequest, "the end is not after the start")
				return
			}
			name = fmt.Sprintf("solastat_history_%s_%s.csv", from.Format("2006-01-02_1504"), end.Format("2006-01-02_1504"))
		} else {
			var errFrom, errTo error
			var to time.Time
			from, errFrom = time.ParseInLocation("2006-01-02", strings.TrimSpace(q.Get("from")), loc)
			to, errTo = time.ParseInLocation("2006-01-02", strings.TrimSpace(q.Get("to")), loc)
			if errFrom != nil || errTo != nil {
				fail(http.StatusBadRequest, "from and to must be dates (YYYY-MM-DD)")
				return
			}
			if to.Before(from) {
				fail(http.StatusBadRequest, "the end date is before the start date")
				return
			}
			end = to.AddDate(0, 0, 1)
			name = fmt.Sprintf("solastat_history_%s_%s.csv", from.Format("2006-01-02"), to.Format("2006-01-02"))
		}
		if end.Sub(from) > time.Duration(history.MaxExportDays)*24*time.Hour+time.Hour {
			fail(http.StatusBadRequest, fmt.Sprintf("export at most %d days at a time", history.MaxExportDays))
			return
		}
		format := history.CSVFormat(q.Get("format"))
		if format != history.CSVExcel {
			format = history.CSVStandard
		}

		w.Header().Set("Content-Type", "text/csv; charset=utf-8")
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, name))
		if err := hs.WriteCSV(r.Context(), w, from, end, loc, format); err != nil {
			// Headers are gone by now; the download just ends short.
			logger.Warnf(logTag, "csv export %s: %v", name, err)
		}
	}))

	mux.HandleFunc("/api/mqtt", logHandler("api/mqtt", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.Method {
		case http.MethodGet:
			cfg := settings.Get().MQTT
			cfg.Password = maskPassword(cfg.Password)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"config": cfg,
				"status": mqttClient.Status(),
			})
		case http.MethodPost:
			var cfg mqttclient.Config
			if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid JSON"})
				return
			}
			if cfg.Password == "••••••••" {
				cfg.Password = settings.Get().MQTT.Password
			}
			if err := settings.Update(func(s *appSettings) { s.MQTT = cfg }); err != nil {
				w.WriteHeader(http.StatusInternalServerError)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
				return
			}
			mqttClient.Reconfigure(cfg)
			_ = json.NewEncoder(w).Encode(map[string]bool{"success": true})
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))

	mux.HandleFunc("/api/mqtt/discover", logHandler("api/mqtt/discover", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if !mqttClient.Status().Connected {
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "MQTT not connected"})
			return
		}
		mqttClient.Reconfigure(settings.Get().MQTT)
		_ = json.NewEncoder(w).Encode(map[string]bool{"success": true})
	}))

	// /api/timezone is the one IANA timezone every date/time-sensitive
	// plugin (currently just smartspot — OMIE's day-boundary shift, price
	// windows, the charge gate) resolves against, instead of each one
	// hardcoding Europe/Lisbon. Set once in the wizard, editable later in
	// Settings; a plugin sees a change immediately, no restart needed. On a
	// system with its own (API 26.2, read by the app's extension) it is that one: a POST
	// sets the system's.
	// Wi-Fi setup (boards with setup mode; other boards answer {"available": false}).
	mux.HandleFunc("/api/wifi/status", logHandler("api/wifi/status", setup.HandleStatus))
	mux.HandleFunc("/api/wifi/scan", logHandler("api/wifi/scan", setup.HandleScan))
	mux.HandleFunc("/api/wifi/connect", logHandler("api/wifi/connect", setup.HandleConnect))
	mux.HandleFunc("/api/wifi/ap", logHandler("api/wifi/ap", setup.HandleAP))

	mux.HandleFunc("/api/timezone", logHandler("api/timezone", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, system := sysZone.get()
		switch r.Method {
		case http.MethodGet:
			// system: the device's own; Settings then shows no timezone to
			// pick, and the wizard's sets the system's.
			_ = json.NewEncoder(w).Encode(map[string]any{"timezone": getTimezone(), "system": system})
		case http.MethodPost:
			var body struct {
				Timezone string `json:"timezone"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid JSON"})
				return
			}
			tz := strings.TrimSpace(body.Timezone)
			if tz == "" {
				tz = defaultTimezone
			}
			if _, err := time.LoadLocation(tz); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": "unknown timezone: " + tz})
				return
			}
			var err error
			if system {
				err = device.SetTimezone(&sysZone, tz)
			} else {
				err = settings.Update(func(s *appSettings) { s.Timezone = tz })
			}
			if err != nil {
				w.WriteHeader(http.StatusInternalServerError)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]bool{"success": true})
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))

	// /api/device-name is the device's name, as people write it ("SolaStat
	// garagem"): what Orbit OS and the Store show. The wizard sets it where
	// the device lets an app do so (API 26.2, through the app's extension: settable); the
	// .local address is the host name and never changes with it.
	nameSettable := device.NameSettable()
	mux.HandleFunc("/api/device-name", logHandler("api/device-name", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.Method {
		case http.MethodGet:
			name, err := c.SystemManager.GetDeviceName()
			if err != nil || name == "Unknown" {
				name = ""
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"name": name, "settable": nameSettable})
		case http.MethodPost:
			var body struct {
				Name string `json:"name"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid JSON"})
				return
			}
			name := strings.TrimSpace(body.Name)
			if err := validDeviceName(name); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
				return
			}
			if !nameSettable {
				w.WriteHeader(http.StatusNotImplemented)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": "this device's name can't be changed here"})
				return
			}
			if err := device.SetName(name); err != nil {
				logger.Warnf(logTag, "device name %q: %v", name, err)
				w.WriteHeader(http.StatusInternalServerError)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
				return
			}
			logger.Infof(logTag, "device name set to %q", name)
			_ = json.NewEncoder(w).Encode(map[string]bool{"success": true})
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))

	// /api/language is the language picked in the web UI, for what the
	// server shows by itself (a board's screen): the pages post it when it
	// changes. Only a language the pages have
	// (web/i18n.js) is taken.
	mux.HandleFunc("/api/language", logHandler("api/language", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.Method {
		case http.MethodGet:
			lang := settings.Get().Language
			if lang == "" {
				lang = "en"
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"language": lang})
		case http.MethodPost:
			var body struct {
				Language string `json:"language"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid JSON"})
				return
			}
			switch body.Language {
			case "en", "pt", "es", "de", "it", "fr":
			default:
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": "unknown language: " + body.Language})
				return
			}
			// Every page posts its language when it loads: write only a change.
			if settings.Get().Language != body.Language {
				if err := settings.Update(func(s *appSettings) { s.Language = body.Language }); err != nil {
					w.WriteHeader(http.StatusInternalServerError)
					_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
					return
				}
				board.Refresh()
			}
			_ = json.NewEncoder(w).Encode(map[string]bool{"success": true})
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))

	// /api/month-start is the day (1–28) the main page's month view starts
	// on, for a month that follows a billing cycle; 1 is the calendar month.
	mux.HandleFunc("/api/month-start", logHandler("api/month-start", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.Method {
		case http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]int{"day": settings.Get().MonthStartDay})
		case http.MethodPost:
			var body struct {
				Day int `json:"day"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid JSON"})
				return
			}
			if body.Day < 1 || body.Day > maxMonthStartDay {
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": fmt.Sprintf("day must be 1-%d", maxMonthStartDay)})
				return
			}
			if err := settings.Update(func(s *appSettings) { s.MonthStartDay = body.Day }); err != nil {
				w.WriteHeader(http.StatusInternalServerError)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]int{"day": body.Day})
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))

	mux.HandleFunc("/api/notifications", logHandler("api/notifications", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.Method {
		case http.MethodGet:
			cfg := getNotifCfg()
			type pluginTypeStatus struct {
				Key     string            `json:"key"`
				Label   string            `json:"label"`
				Labels  map[string]string `json:"labels,omitempty"`
				Enabled bool              `json:"enabled"`
			}
			pluginTypes := []pluginTypeStatus{}
			for _, nt := range collectNotifTypes() {
				enabled := nt.DefaultEnabled
				if v, ok := cfg.PluginTypes[nt.Key]; ok {
					enabled = v
				}
				pluginTypes = append(pluginTypes, pluginTypeStatus{Key: nt.Key, Label: nt.Label, Labels: nt.Labels, Enabled: enabled})
			}
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"config":       cfg,
				"plugin_types": pluginTypes,
			})
		case http.MethodPost:
			var cfg NotificationsConfig
			if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid JSON"})
				return
			}
			if cfg.BatteryLowThreshold <= 0 {
				cfg.BatteryLowThreshold = DefaultNotificationsConfig().BatteryLowThreshold
			}
			if err := settings.Update(func(s *appSettings) { s.Notifications = cfg }); err != nil {
				w.WriteHeader(http.StatusInternalServerError)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]bool{"success": true})
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))

	// ── Configuration backup / restore (see config_backup.go) ────────────
	currentConfig := func() configState {
		stateMu.RLock()
		id, port := activeProfileID, activePort
		stateMu.RUnlock()
		cur := settings.Get()
		return configState{
			ActiveProfile: id,
			UARTPort:      port,
			Timezone:      getTimezone(),
			MonthStartDay: cur.MonthStartDay,
			SetupDone:     cur.SetupDone,
			Notifications: cur.Notifications,
			MQTT:          cur.MQTT,
		}
	}

	// GET /api/backup downloads the configuration as JSON (MQTT password included).
	mux.HandleFunc("/api/backup", logHandler("api/backup", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		b, err := buildConfigBackup(inverterDir, appManifest.Version, currentConfig())
		if err != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		name := fmt.Sprintf("solastat-config-%s.json", time.Now().In(appLocation()).Format("2006-01-02"))
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, name))
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		_ = enc.Encode(b)
	}))

	// POST /api/restore applies a configuration backup. The whole file is
	// validated first; the current configuration is saved in the data dir
	// as config_pre_restore.json so a restore can be undone by restoring
	// that file.
	var restoreMu sync.Mutex
	mux.HandleFunc("/api/restore", logHandler("api/restore", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		fail := func(status int, msg string) {
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
		}
		var b configBackup
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxConfigBackupBytes)).Decode(&b); err != nil {
			fail(http.StatusBadRequest, "not a valid backup file: "+err.Error())
			return
		}
		if err := b.validate(); err != nil {
			fail(http.StatusBadRequest, err.Error())
			return
		}
		if b.ActiveProfile != "" {
			if _, ok := b.Profiles[b.ActiveProfile]; !ok {
				if _, err := loadProfile(b.ActiveProfile, inverterDir); err != nil {
					fail(http.StatusBadRequest, fmt.Sprintf("the backup's active profile %q is not in it nor on this device", b.ActiveProfile))
					return
				}
			}
		}

		restoreMu.Lock()
		defer restoreMu.Unlock()

		if prev, err := buildConfigBackup(inverterDir, appManifest.Version, currentConfig()); err == nil {
			if data, err := json.MarshalIndent(prev, "", "  "); err == nil {
				if err := fsutil.WriteFileAtomic(filepath.Join(dataDir, configPreRestoreFile), data, 0o600); err != nil {
					logger.Warnf(logTag, "restore: save current configuration: %v", err)
				}
			}
		}

		ports, _ := c.UartManager.ListPorts()
		port, warnings, err := b.apply(inverterDir, settings, ports)
		if err != nil {
			fail(http.StatusInternalServerError, err.Error())
			return
		}

		// Then put them to work, as Settings would.
		mqttClient.Reconfigure(settings.Get().MQTT)
		if port != "" {
			stateMu.Lock()
			activePort = port
			stateMu.Unlock()
		}
		if b.ActiveProfile != "" {
			if err := applyProfile(b.ActiveProfile); err != nil {
				warnings = append(warnings, "profile: "+err.Error())
			}
		}
		logger.Infof(logTag, "configuration restored from a backup of %s (%d profiles, %d warnings)", b.Created.Format(time.RFC3339), len(b.Profiles), len(warnings))
		eventLog.Add(eventlog.Entry{Ts: time.Now(), Source: "core", Kind: "config_restored", Text: "Configuration restored from a backup"})
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "warnings": warnings})
	}))

	// The pages, zipped once and with an ETag (webgz.Asset): no-cache so
	// they always match /i18n.js and /nav.js, a 304 when nothing changed.
	settingsPage := webgz.NewAsset("text/html; charset=utf-8", settingsHTML)
	mux.HandleFunc("/settings", logHandler("settings-page", settingsPage.ServeHTTP))

	wizardPage := webgz.NewAsset("text/html; charset=utf-8", wizardHTML)
	mux.HandleFunc("/wizard", logHandler("wizard-page", wizardPage.ServeHTTP))

	// /api/setup/complete marks first-run setup as done, so "/" stops
	// redirecting to the wizard. The wizard itself never configures
	// anything through this endpoint — it drives the same /api/profile,
	// /api/ports and /api/mqtt endpoints Settings already uses, and only
	// calls this once, on its last step, to record that a human actually
	// went through the flow at least once.
	mux.HandleFunc("/api/setup/complete", logHandler("api/setup/complete", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if err := settings.Update(func(s *appSettings) { s.SetupDone = true }); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]bool{"success": true})
	}))

	// /api/setup/connect is the wizard's "connection confirmed" step: it opens
	// the inverter connection for the profile/port chosen so far, so step 3
	// can test it. Until then nothing talks to the inverter (see applyProfile).
	mux.HandleFunc("/api/setup/connect", logHandler("api/setup/connect", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		stateMu.RLock()
		currentID := activeProfileID
		stateMu.RUnlock()
		if err := switchProfile(currentID); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]bool{"success": true})
	}))

	// POST /api/setup/reset puts the device back to a fresh install and hands it
	// to the wizard: it deletes the history and every configuration (app
	// settings, inverter profiles back to the built-in ones, plugin
	// settings via their Reset, the activity log), keeping only the license
	// keys and the Wi-Fi. Settings asks for confirmation first.
	mux.HandleFunc("/api/setup/reset", logHandler("api/setup/reset", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		restoreMu.Lock()
		defer restoreMu.Unlock()
		var warnings []string
		warn := func(what string, err error) {
			logger.Warnf(logTag, "reset: %s: %v", what, err)
			warnings = append(warnings, what+": "+err.Error())
		}

		// Nothing may keep writing while things are deleted.
		stopConnection()
		setHistoryStore(nil)
		warnings = append(warnings, deleteForReset(dataDir, inverterDir, historyDBPath)...)

		if err := settings.Update(func(s *appSettings) { *s = defaultSettings() }); err != nil {
			warn("reset settings", err)
		}
		mqttClient.Reconfigure(settings.Get().MQTT)

		for _, p := range allPlugins {
			if rp, ok := p.(interface{ Reset() error }); ok {
				if err := rp.Reset(); err != nil {
					warn("reset plugin "+p.Info().ID, err)
				}
			}
		}

		eventLog.Clear()
		latestMu.Lock()
		latest = solar.Data{Readings: map[string]solar.Reading{}}
		comm = commHealth{}
		latestMu.Unlock()

		const builtinDefaultProfile = "swatten" // matches the --profile flag's own default
		if err := applyProfile(builtinDefaultProfile); err != nil {
			warn("select the default profile", err)
		}
		logger.Infof(logTag, "reset to a fresh install (%d warnings)", len(warnings))
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "warnings": warnings})
	}))

	// GET /api/inverter/restart says whether the active inverter can be
	// restarted from here (the dashboard shows its button only then); POST
	// restarts it. The dashboard asks for confirmation first.
	mux.HandleFunc("/api/inverter/restart", logHandler("api/inverter/restart", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		supported := inverterRestart != nil && inverterRestart.SupportsRestart()
		switch r.Method {
		case http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]bool{"supported": supported})
		case http.MethodPost:
			if !supported {
				w.WriteHeader(http.StatusConflict)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": "the active inverter can't be restarted from SolaStat"})
				return
			}
			if err := inverterRestart.Restart(); err != nil {
				logger.Warnf(logTag, "restart inverter: %v", err)
				w.WriteHeader(http.StatusBadGateway)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
				return
			}
			notifEdge.mu.Lock()
			notifEdge.quietUntil = time.Now().Add(inverterRestartQuiet)
			notifEdge.mu.Unlock()
			eventLog.Add(eventlog.Entry{Ts: time.Now(), Source: "core", Kind: "inverter_restarted", Text: "Inverter restarted from the dashboard"})
			_ = json.NewEncoder(w).Encode(map[string]bool{"success": true})
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))

	// Every compiled-in plugin registers its routes directly on this same
	// mux — no reverse proxy, no separate HTTP listener: it's the same
	// process. A plugin nobody has a key for still registers its routes
	// (its own handlers gate on authorization internally, same as
	// before), so this is unconditional, not tied to Status().
	for _, p := range allPlugins {
		p.RegisterRoutes(mux)
	}

	// /api/plugins lists only plugins that currently report themselves
	// authorized — there's no fixed catalog, a plugin nobody has a key for
	// simply never appears here, compiled in or not. "keys" is the raw
	// stored pool (opaque strings, same ones pasted into Settings) — for
	// managing/removing one, not for judging what's authorized; the core
	// still never decides that itself.
	mux.HandleFunc("/api/plugins", logHandler("api/plugins", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"device_hash": pluginStore.DeviceHash(),
			"plugins":     pluginStore.Status(),
			"keys":        pluginStore.Keys(),
			// certified: the unit's plugins come licensed, so Settings asks
			// for no key.
			"certified": unit.Certified,
		})
	}))

	// /api/plugins/activate accepts one generic key (it names its own
	// target plugin(s)) and just stores it; the core
	// never verifies it itself.
	mux.HandleFunc("/api/plugins/activate", logHandler("api/plugins/activate", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			Key string `json:"key"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Key) == "" {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid body, expected {\"key\":\"...\"}"})
			return
		}
		if err := pluginStore.Activate(strings.TrimSpace(req.Key)); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]bool{"success": true})
	}))

	// /api/plugins/deactivate removes one previously-pasted key from the
	// pool (the reverse of activate) — same "just stores/removes an opaque
	// string" contract, no verification.
	mux.HandleFunc("/api/plugins/deactivate", logHandler("api/plugins/deactivate", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			Key string `json:"key"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Key) == "" {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid body, expected {\"key\":\"...\"}"})
			return
		}
		if err := pluginStore.Deactivate(strings.TrimSpace(req.Key)); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]bool{"success": true})
	}))

	// /api/logs serves the dashboard's activity log — inverter status,
	// relay on/off, smartspot write attempts, all merged into the one
	// in-RAM eventLog ring buffer (see eventLogCapacity). Paginated
	// newest-first; nothing here survives a restart.
	mux.HandleFunc("/api/logs", logHandler("api/logs", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		limit := 10
		if v := strings.TrimSpace(r.URL.Query().Get("limit")); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				limit = n
			}
		}
		if limit > 100 {
			limit = 100
		}
		offset := 0
		if v := strings.TrimSpace(r.URL.Query().Get("offset")); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n >= 0 {
				offset = n
			}
		}
		events, total := eventLog.Recent(limit, offset)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"events": events,
			"total":  total,
		})
	}))

	mux.HandleFunc("/events", logHandler("events", sseHandler(hub, func() solar.Data {
		latestMu.RLock()
		defer latestMu.RUnlock()
		return latest
	})))

	favicon := webgz.NewAsset("image/svg+xml", iconSVG)
	mux.HandleFunc("/favicon.svg", func(w http.ResponseWriter, r *http.Request) {
		favicon.ServeHTTP(w, r)
	})

	// /i18n.js (the translations shared by every page) and /nav.js are
	// loaded by pages without a version in the URL, so a cached copy from
	// before an update would pair old translations with new pages (raw keys
	// like "card_reset" on screen). webgz.Asset's no-cache makes the browser
	// check every time; the ETag keeps that check to a tiny 304 when nothing
	// changed.
	mux.Handle("/nav.js", webgz.NewAsset("application/javascript; charset=utf-8", navJS))
	mux.Handle("/i18n.js", webgz.NewAsset("application/javascript; charset=utf-8", i18nJS))

	dashboardPage := webgz.NewAsset("text/html; charset=utf-8", dashboardHTML)

	// Catch-all for root LAST
	mux.HandleFunc("/", logHandler("root", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			logger.Infof(logTag, "404: %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		if !setupDone() {
			http.Redirect(w, r, "/wizard", http.StatusFound)
			return
		}
		logger.Infof(logTag, "serving dashboard")
		dashboardPage.ServeHTTP(w, r)
	}))

	ln, actualPort := listenWithFallback(*httpPort)
	srv := &http.Server{
		// webgz: every text response gzipped for a browser that takes it.
		Handler:           webgz.Handler(protectCrossOrigin(setup.Middleware(requireSetup(setupDone, mux)))),
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		logger.Infof(logTag, "Dashboard listening on %s:%d, reachable at /solastat through the Orbit OS launcher", httpListenHost, actualPort)
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			logger.Errorf(logTag, "http server: %v", err)
		}
	}()

	// ── Register with AppHub so the portal shows a link to the dashboard ─────
	if err := c.AppHubManager.RegisterService(&apphub.RegisterServiceRequest{
		Host:   "127.0.0.1",
		Port:   int32(actualPort),
		Routes: []*apphub.Route{{Path: "/solastat"}},
		Health: &apphub.HealthCheck{
			Type: apphub.HealthCheckType_HEALTH_CHECK_TCP,
		},
	}); err != nil {
		logger.Errorf(logTag, "apphub register: %v", err)
	} else {
		logger.Infof(logTag, "Registered with AppHub at /solastat -> 127.0.0.1:%d", actualPort)
	}
	defer c.AppHubManager.UnregisterService()

	<-ctx.Done()
	logger.Infof(logTag, "Shutting down…")

	// Close UART and databases immediately on SIGTERM, before the HTTP server
	// drains (which can block up to 5 s). This ensures the port is released and
	// all data is flushed before the update process starts the new instance.
	stateMu.Lock()
	if pollCancel != nil {
		pollCancel()
		pollCancel = nil
	}
	if mb != nil {
		mb.Stop()
		mb = nil
	}
	stateMu.Unlock()
	setHistoryStore(nil)

	shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	srv.Shutdown(shutCtx)
}

// sseHandler returns an HTTP handler that streams solar.Data updates via SSE.
func sseHandler(hub *broker, getLatest func() solar.Data) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "SSE not supported", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("Access-Control-Allow-Origin", "*")

		ch := hub.subscribe()
		defer hub.unsubscribe(ch)

		// Send the current snapshot immediately so the dashboard isn't blank.
		if b, err := json.Marshal(getLatest()); err == nil {
			fmt.Fprintf(w, "data: %s\n\n", b)
			flusher.Flush()
		}

		// 10s rather than a more typical 25-30s: when accessed through the
		// AppHub remote-access tunnel, connections have been observed
		// dropping (EventSource firing its native error) well under 25s of
		// idle time — a shorter beat gives that relay hop less room to ever
		// see a gap long enough to recycle the connection.
		keepalive := time.NewTicker(10 * time.Second)
		defer keepalive.Stop()

		for {
			select {
			case msg, ok := <-ch:
				if !ok {
					return
				}
				fmt.Fprintf(w, "data: %s\n\n", msg)
				flusher.Flush()
			case <-keepalive.C:
				fmt.Fprintf(w, ": keepalive\n\n")
				flusher.Flush()
			case <-r.Context().Done():
				return
			}
		}
	}
}

// loadProfile resolves the profile from <profilesDir>/<id>.json.
// protectCrossOrigin refuses state-changing requests (POST, DELETE...) sent
// by a page from another site, e.g. a malicious page open in a browser on
// the same network. The app's own pages, direct or through the Orbit OS
// launcher, are same-origin; tools like curl send no browser headers and
// are let through.
func protectCrossOrigin(h http.Handler) http.Handler {
	return http.NewCrossOriginProtection().Handler(h)
}

// requireSetup sends every page opened in the browser (Settings, a plugin's
// page...) to the wizard until first-run setup is done, like "/" does: those
// pages aren't usable before an inverter is picked. Only page loads are
// redirected, told apart by the browser asking for HTML; the wizard's own
// API calls, scripts, icons and the event stream pass through.
func requireSetup(done func() bool, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !done() && r.Method == http.MethodGet && r.URL.Path != "/wizard" &&
			!strings.HasPrefix(r.URL.Path, "/api/") &&
			strings.Contains(r.Header.Get("Accept"), "text/html") {
			http.Redirect(w, r, "/wizard", http.StatusFound)
			return
		}
		h.ServeHTTP(w, r)
	})
}

func loadProfile(id, dir string) (*profile.Profile, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, fmt.Errorf("missing profile id")
	}
	// Same rule as saveProfile/deleteProfile: an id is a file name, never a
	// path, so a request can't load any .json file on the device.
	if !profileIDPattern.MatchString(id) {
		return nil, fmt.Errorf("invalid profile id %q", id)
	}
	if dir == "" {
		return nil, fmt.Errorf("profiles dir is empty")
	}
	p, err := profile.LoadFile(filepath.Join(dir, id+".json"))
	if err != nil {
		return nil, fmt.Errorf("load profile %q: %w", id, err)
	}
	// A built-in profile seeded before fault_state, the fault registers (or
	// the voltage and current readings) existed is never rewritten
	// (ensureDefaultProfiles), so take what it lacks from the embedded copy.
	for _, b := range builtinProfiles() {
		if b.id != id {
			continue
		}
		bp, err := profile.Load(b.data)
		if err != nil {
			break
		}
		if p.FaultState == nil {
			p.FaultState = bp.FaultState
		}
		if len(p.Faults) == 0 {
			p.Faults = bp.Faults
		}
		if len(p.StatusNames) == 0 {
			p.StatusNames = bp.StatusNames
		}
		p.Registers, p.Derived = withAddedReadings(p.Registers, p.Derived, bp.Registers, bp.Derived)
	}
	return p, nil
}

// addedReadings are the readings built-in profiles gained after a device
// may have seeded its copy: the history's voltages, currents and grid
// frequency.
var addedReadings = map[string]bool{
	"grid_voltage": true, "grid_current": true,
	"grid_voltage_l2": true, "grid_current_l2": true,
	"grid_voltage_l3": true, "grid_current_l3": true,
	"grid_frequency":      true,
	"pv1_total_power_p_v": true, "pv1_total_power_p_i": true,
	"pv2_total_power_p_v": true, "pv2_total_power_p_i": true,
	"pv3_total_power_p_v": true, "pv3_total_power_p_i": true,
	"pv4_total_power_p_v": true, "pv4_total_power_p_i": true,
	"battery_voltage": true, "battery_current": true,
}

// withAddedReadings appends to regs and derived the builtin ones among
// addedReadings that the profile lacks, registers at the end: a failed read
// ends the poll cycle, so a register a model turns out not to have never
// costs the readings before it.
func withAddedReadings(regs []profile.Register, derived []profile.DerivedRegister,
	builtinRegs []profile.Register, builtinDerived []profile.DerivedRegister) ([]profile.Register, []profile.DerivedRegister) {
	have := make(map[string]bool, len(regs)+len(derived))
	for _, r := range regs {
		have[r.Name] = true
	}
	for _, d := range derived {
		have[d.Name] = true
	}
	for _, r := range builtinRegs {
		if addedReadings[r.Name] && !have[r.Name] {
			regs = append(regs, r)
		}
	}
	for _, d := range builtinDerived {
		if addedReadings[d.Name] && !have[d.Name] {
			derived = append(derived, d)
		}
	}
	return regs, derived
}

// listenWithFallback tries to bind on preferredPort; if that's in use it tries
// successive ports (preferredPort+1, +2, ...) until one is free. Returns the
// listener and the actual port number.
//
// It binds to loopback only: the dashboard has no login of its own and is
// reached through the Orbit OS launcher's proxy (/solastat), which requires
// the launcher's login. Listening on every interface would let anyone on the
// network open the dashboard directly on its port, bypassing that login.
// httpPortMin..httpPortMax is the range the dashboard's HTTP listener is
// allowed to use. The preferred port (the -http-port flag) is clamped into
// it, and if that port is taken the next free one is used, wrapping round
// to httpPortMin, so the dashboard never lands outside the range.
const (
	httpListenHost = "127.0.0.1" // loopback only, see listenWithFallback
	httpPortMin    = 50000
	httpPortMax    = 60000
)

// resolveDeviceSerial returns the device's identity: the CPU serial (on a
// Raspberry Pi, burnt into the SoC, so it survives reinstalling the OS). It
// must be unique per device: plugin licenses are bound to it and the device's
// setup network is named and keyed from it. A placeholder ("unknown", all
// zeros; see usableSerial) is reported as an error rather than used.
func resolveDeviceSerial(c *client.Client) (serial, source string) {
	v, err := c.SystemManager.GetCpuSerial()
	if err != nil {
		logger.Errorf(logTag, "read CPU serial: %v", err)
		return "", "nothing"
	}
	if !usableSerial(v) {
		logger.Errorf(logTag, "CPU serial is not usable (%q)", v)
		return "", "nothing"
	}
	return strings.TrimSpace(v), "CPU serial"
}

// usableSerial rejects the placeholders boards return when they have no
// real serial: empty, "unknown"/"none"/"n/a"-style words, and values made of
// a single repeated character (all zeros, all Fs).
func usableSerial(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "", "unknown", "none", "null", "n/a", "na", "default", "not specified", "to be filled by o.e.m.":
		return false
	}
	var chars []rune
	for _, r := range strings.ToLower(v) {
		if (r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') {
			chars = append(chars, r)
		}
	}
	if len(chars) < 4 {
		return false
	}
	for _, r := range chars[1:] {
		if r != chars[0] {
			return true
		}
	}
	return false
}

// deviceHost is the device's name on the network, its host name in the
// ".local" domain, "" when the system doesn't tell it.
func deviceHost(d extapi.Device) string {
	name, err := d.HostName()
	if err != nil {
		logger.Warnf(logTag, "host name: %v", err)
		return ""
	}
	return localHost(name)
}

// localHost is name in the ".local" domain, "" for the runtime's "Unknown".
func localHost(name string) string {
	if name == "" || name == "Unknown" {
		return ""
	}
	return strings.TrimSuffix(name, ".local") + ".local"
}

// wiredUp reports whether the device is on a wired network: an Ethernet
// interface connected with an address.
func wiredUp(c *client.Client) bool {
	eths, err := c.EthernetManager.ListEthernetInterfaces()
	if err != nil {
		return false
	}
	for _, e := range eths {
		if e.GetState() == ethsvc.EthernetState_ETH_CONNECTED && e.GetIpv4Address() != "" {
			return true
		}
	}
	return false
}

// validDeviceName checks a device name as Orbit OS takes it (API 26.2's
// SetDeviceName): 1 to 63 characters, none of them a control character.
func validDeviceName(name string) error {
	n := utf8.RuneCountInString(name)
	if n == 0 {
		return errors.New("the name is empty")
	}
	if n > 63 {
		return errors.New("the name is longer than 63 characters")
	}
	if !utf8.ValidString(name) {
		return errors.New("the name is not valid text")
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return errors.New("the name has a control character")
		}
	}
	return nil
}

// edition is what the logo says next to SolaStat: the unit's edition, DIY
// by default.
func edition(u extapi.Unit) string {
	if u.Edition == "" {
		return "DIY"
	}
	return u.Edition
}

func listenWithFallback(preferredPort int) (net.Listener, int) {
	if preferredPort < httpPortMin || preferredPort > httpPortMax {
		logger.Infof(logTag, "port %d is outside %d-%d, using %d", preferredPort, httpPortMin, httpPortMax, httpPortMin)
		preferredPort = httpPortMin
	}
	span := httpPortMax - httpPortMin + 1
	for i := 0; i < span; i++ {
		port := httpPortMin + (preferredPort-httpPortMin+i)%span
		ln, err := net.Listen("tcp", fmt.Sprintf("%s:%d", httpListenHost, port))
		if err == nil {
			if port != preferredPort {
				logger.Infof(logTag, "port %d in use, using port %d instead", preferredPort, port)
			}
			return ln, port
		}
		logger.Infof(logTag, "port %d in use, trying next port", port)
	}
	logger.Fatalf(logTag, "could not bind HTTP listener: no free port in range %d-%d", httpPortMin, httpPortMax)
	os.Exit(1)
	return nil, 0
}

// resolveUartPort returns the port to use for Modbus:
//   - If override is non-empty and present in the device list, use it.
//   - If override is empty and exactly one port is available, use that.
//   - If override is empty and multiple ports are available, default to the first one.
func resolveUartPort(um *client.UartManager, override string) (string, error) {
	ports, err := um.ListPorts()
	if err != nil {
		return "", fmt.Errorf("list UART ports: %w", err)
	}
	logger.Infof(logTag, "Available UART ports: %v", ports)

	if override != "" {
		for _, p := range ports {
			if p == override {
				return override, nil
			}
		}
		return "", fmt.Errorf("port %q not found on device (available: %v)", override, ports)
	}

	if len(ports) == 1 {
		logger.Infof(logTag, "Auto-selected UART port: %s", ports[0])
		return ports[0], nil
	}
	if len(ports) == 0 {
		return "", fmt.Errorf("no UART ports available on device")
	}

	// Neutral selection: pick the first available port as reported by the device.
	logger.Warnf(logTag, "Multiple UART ports available %v; defaulting to first: %s", ports, ports[0])
	return ports[0], nil
}

var profileIDPattern = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

// controlProfileID is the profile ID the inverter control picks its driver
// by: the profile's base when it's a copy of a built-in, else its own ID.
func controlProfileID(id string, p *profile.Profile) string {
	if p != nil && p.Base != "" {
		return p.Base
	}
	return id
}

type profileSummary struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	Source        string `json:"source"`
	RegisterCount int    `json:"register_count"`
}

func listProfiles(dir string) ([]profileSummary, error) {
	if dir == "" {
		return nil, fmt.Errorf("profiles dir is empty")
	}
	items := []profileSummary{}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return []profileSummary{}, nil
		}
		return nil, fmt.Errorf("read profiles dir: %w", err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		p, perr := profile.LoadFile(filepath.Join(dir, e.Name()))
		if perr != nil {
			continue
		}
		items = append(items, profileSummary{
			ID:            strings.TrimSuffix(e.Name(), filepath.Ext(e.Name())),
			Name:          p.Name,
			Source:        "file",
			RegisterCount: len(p.Registers),
		})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].ID < items[j].ID })
	return items, nil
}

func saveProfile(dir, requestedID string, p *profile.Profile) error {
	id := strings.TrimSpace(requestedID)
	if id == "" {
		id = slugFromName(p.Name)
	}
	if id == "" {
		return fmt.Errorf("profile id is required (or provide a valid name)")
	}
	if !profileIDPattern.MatchString(id) {
		return fmt.Errorf("profile id must match %s", profileIDPattern.String())
	}
	if strings.TrimSpace(p.Name) == "" {
		return fmt.Errorf("profile name is required")
	}
	if len(p.Registers) == 0 {
		return fmt.Errorf("profile must define at least one register")
	}
	p.Base = strings.TrimSpace(p.Base)
	if p.Base != "" && !profileIDPattern.MatchString(p.Base) {
		return fmt.Errorf("profile base must match %s", profileIDPattern.String())
	}
	if p.Uart.BaudRate <= 0 || p.Uart.DataBits <= 0 || p.Uart.StopBits <= 0 {
		return fmt.Errorf("invalid uart config")
	}
	switch strings.ToLower(strings.TrimSpace(p.Uart.Parity)) {
	case "none", "even", "odd":
	default:
		return fmt.Errorf(`uart.parity must be "none", "even", or "odd"`)
	}
	// A network profile (Solarman, Modbus TCP or RTU over TCP) can't connect
	// without a valid address: refuse it here rather than failing later,
	// when the app tries to connect. The Solarman serial number is optional,
	// and so is the port (0 = 502, or 8899 for RTU over TCP).
	switch p.Modbus.Connection {
	case "", profile.ConnRTU:
	case profile.ConnSolarman, profile.ConnModbusTCP, profile.ConnRTUOverTCP:
		what := "the Solarman datalogger"
		switch p.Modbus.Connection {
		case profile.ConnModbusTCP:
			what = "the inverter (Modbus TCP)"
		case profile.ConnRTUOverTCP:
			what = "the Modbus RTU over TCP gateway"
		}
		host := strings.TrimSpace(p.Modbus.TCPHost)
		if host == "" {
			return fmt.Errorf("%s: IP address is empty", what)
		}
		if !validHost(host) {
			return fmt.Errorf("%q is not a valid IP address or host name for %s", host, what)
		}
		p.Modbus.TCPHost = host
	default:
		return fmt.Errorf(`modbus.connection must be "rtu", "tcp", "modbus_tcp" or "rtu_tcp"`)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create profiles dir: %w", err)
	}
	out, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return fmt.Errorf("encode profile: %w", err)
	}
	path := filepath.Join(dir, id+".json")
	if err := fsutil.WriteFileAtomic(path, append(out, '\n'), 0o644); err != nil {
		return fmt.Errorf("write profile file: %w", err)
	}
	return nil
}

func slugFromName(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" {
		return ""
	}
	var b strings.Builder
	lastDash := false
	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			lastDash = false
			continue
		}
		if !lastDash {
			b.WriteByte('-')
			lastDash = true
		}
	}
	s := strings.Trim(b.String(), "-")
	return s
}

func deleteProfile(dir, id string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return fmt.Errorf("missing profile id")
	}
	if !profileIDPattern.MatchString(id) {
		return fmt.Errorf("invalid profile id")
	}
	path := filepath.Join(dir, id+".json")
	if err := os.Remove(path); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("profile %q not found", id)
		}
		return fmt.Errorf("remove profile: %w", err)
	}
	return nil
}

// hostNamePattern is a DNS host name: dot-separated labels of letters,
// digits and hyphens, not starting or ending with a hyphen.
var hostNamePattern = regexp.MustCompile(`^(?i)[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)*$`)

// validHost accepts an IPv4 or IPv6 address, or a host name. Anything made
// only of digits and dots must be a whole IPv4 address, so "192.168.1" or
// "192.168.1.300" are refused rather than taken for host names.
func validHost(host string) bool {
	if net.ParseIP(host) != nil {
		return true
	}
	if strings.Trim(host, "0123456789.") == "" {
		return false
	}
	return len(host) <= 253 && hostNamePattern.MatchString(host)
}

// builtinProfileIDs lists the profiles embedded in the binary, in the order
// they are seeded. The ones from swatten to sofar were written for solastat;
// the rest are converted from ha-solarman's inverter definitions
// (github.com/davidrapan/ha-solarman) and still untested here.
var builtinProfileIDs = []string{
	"swatten", "deye_sg04lp3", "solis", "huawei", "goodwe_et", "sofar",
	"afore_hybrid", "afore_t4_string", "afore_t6_string",
	"anenji_hybrid",
	"astronergy_micro",
	"chint_hybrid",
	"deye_hybrid", "deye_micro", "deye_string",
	"hinen_hybrid",
	"invt_xd",
	"kstar_hybrid",
	"maxge_string",
	"megarevo_r3h",
	"renon_ifl",
	"sofar_g3", "sofar_hyd_es", "sofar_string",
	"solis_3p_hybrid", "solis_string",
	"renac_esc",
	"srne_asf",
	"tsun_micro",
}

// builtinProfiles lists the profiles embedded in the binary, by id.
func builtinProfiles() []struct {
	id   string
	data []byte
} {
	out := make([]struct {
		id   string
		data []byte
	}, 0, len(builtinProfileIDs))
	for _, id := range builtinProfileIDs {
		data, err := builtinProfilesFS.ReadFile("orb/data/inverters/" + id + ".json")
		if err != nil {
			panic(fmt.Sprintf("built-in profile %s: %v", id, err)) // a missing file is a build mistake
		}
		out = append(out, struct {
			id   string
			data []byte
		}{id, data})
	}
	return out
}

// ensureDefaultProfiles writes each built-in profile into dir if it isn't
// there yet. An existing file is never overwritten: it may have been edited
// (registers, or the connection — a Solarman datalogger's IP and serial).
func ensureDefaultProfiles(dir string) error {
	if strings.TrimSpace(dir) == "" {
		return fmt.Errorf("profiles dir is empty")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create profiles dir: %w", err)
	}
	for _, b := range builtinProfiles() {
		path := filepath.Join(dir, b.id+".json")
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			continue
		}
		if err := fsutil.WriteFileAtomic(path, append(b.data, '\n'), 0o644); err != nil {
			return fmt.Errorf("seed %s profile: %w", b.id, err)
		}
	}
	return nil
}

// readingValue returns the value of the named reading in d, or NaN if missing.
func readingValue(d solar.Data, name string) float64 {
	if d.Readings == nil {
		return nan
	}
	r, ok := d.Readings[name]
	if !ok {
		return nan
	}
	return r.Value
}

var nan = func() float64 {
	zero := 0.0
	return zero / zero
}()

func resolveDataDir(flagValue string) (string, error) {
	if strings.TrimSpace(flagValue) != "" {
		return filepath.Clean(flagValue), nil
	}

	// Local dev, running from the repo root or from cmd/solastat: the data
	// live in devdata/ (gitignored), never under orb/. Everything in orb/
	// is packed into the .orb as it is, so a settings.json there would ship
	// with setup_done (no wizard on a fresh install), and the profiles the
	// app seeds and edits there are the source of the built-in ones.
	for _, dev := range []struct{ src, data string }{
		{"cmd/solastat/orb", "cmd/solastat/devdata"},
		{"orb", "devdata"},
	} {
		if st, err := os.Stat(filepath.Join(dev.src, "data", invertersDirName)); err == nil && st.IsDir() {
			return filepath.Clean(dev.data), nil
		}
	}

	// Installed OrbitOS app: confirmed on-device that the running binary's
	// own working directory already IS its private data directory — the
	// .orb packager extracts this repo's cmd/solastat/orb/data straight
	// into it (subdirectories included), not into a further "data"
	// subfolder underneath. That was confirmed against the
	// sdk.dev.solastat package, before this project's second rename
	// (solastat -> solastat, see orbit.project.json); the package id
	// changes with this rename, so re-confirm the extraction path (the
	// "Directory scan" panel in Settings, or direct inspection of the
	// device's filesystem) after the next on-device install if anything
	// looks off — the mechanism itself (CWD == data dir) isn't expected to
	// change.
	return ".", nil
}

// NotificationsConfig controls which events push a mobile notification via
// the SDK's MobileNotificationManager. Enabled is the master switch — every
// individual type is still gated behind it, so turning notifications off
// doesn't require unchecking each one. Defaults to everything on, Enabled
// included, so the feature works out of the box without a trip to Settings.
//
// PluginTypes holds the user's override for plugin-declared notification
// types (see pluginapi.NotificationTypeProvider), keyed by "<plugin
// ID>:<type ID>" — deliberately NOT one struct field per plugin. A key
// missing from this map (e.g. a plugin installed after the user last saved
// this page) falls back to that plugin's own DefaultEnabled, not false —
// see collectNotifTypes's callers in main().
type NotificationsConfig struct {
	Enabled             bool            `json:"enabled"`
	BatteryFull         bool            `json:"battery_full"`
	BatteryLow          bool            `json:"battery_low"`
	BatteryLowThreshold float64         `json:"battery_low_threshold"`
	CommError           bool            `json:"comm_error"`
	DeviceRecovered     bool            `json:"device_recovered"`
	InverterFault       bool            `json:"inverter_fault"`
	PluginTypes         map[string]bool `json:"plugin_types"`
}

// notifPluginType is one plugin-declared notification type, resolved to
// its fully-namespaced settings key — see collectNotifTypes in main().
type notifPluginType struct {
	Key            string            `json:"key"`
	Label          string            `json:"label"`
	Labels         map[string]string `json:"labels,omitempty"` // Label translated, by language code
	DefaultEnabled bool              `json:"default_enabled"`
}

func DefaultNotificationsConfig() NotificationsConfig {
	return NotificationsConfig{
		Enabled:             true,
		BatteryFull:         true,
		BatteryLow:          true,
		BatteryLowThreshold: 20,
		CommError:           true,
		DeviceRecovered:     true,
		InverterFault:       true,
		PluginTypes:         map[string]bool{},
	}
}

func maskPassword(p string) string {
	if p == "" {
		return ""
	}
	return "••••••••"
}
