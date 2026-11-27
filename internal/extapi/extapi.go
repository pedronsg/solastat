// Package extapi is what the app and its extension (the solastat/ext
// module) say to each other: what the app hands the extension, and what
// the extension gives back. The ext module next to this one is the
// default extension, which adds nothing; a build may replace it (a go.work
// replace of solastat/ext) with one that adds a board's hardware, plugins
// or a newer device API.
package extapi

import (
	"context"
	"net/http"
	"time"

	"solastat/internal/history"
	"solastat/internal/setupmode"
	"solastat/internal/solar"

	"github.com/pedronsg/solastat-template/pkg/pluginapi"

	"github.com/OrbitOS-org/orbit-os-sdk-go/v26/client"
)

// Device is what the device's system offers beyond what the app reads
// itself: the setup network, the product, the names and the time zone.
type Device interface {
	// OpenSetupAP opens setup mode's network; nil: setup mode opens it with
	// the SDK's StartAP.
	OpenSetupAP() func(ifname, ssid string, portal bool) error
	// Unit is what this unit is; boardModel is the board's model.
	Unit(boardModel string) Unit
	// HostName is the device's host name.
	HostName() (string, error)
	// NameSettable reports whether SetName can set the device's name.
	NameSettable() bool
	// SetName sets the device's name, as people write it.
	SetName(name string) error
	// WatchTimezone keeps the system's own time zone in z, where the system
	// has one; z stays unset where it has none.
	WatchTimezone(ctx context.Context, z Zone)
	// SetTimezone sets the system's own time zone, and z with it.
	SetTimezone(z Zone, tz string) error
}

// Unit is what the extension knows the unit to be. The zero Unit is any
// board the extension doesn't know: the DIY edition, nothing fixed.
type Unit struct {
	// Product is the unit's product id, handed on to the plugins; "" for
	// none.
	Product string
	// Edition is what the logo says next to the app's name; "" is "DIY".
	Edition string
	// Certified: its plugins come licensed, without a key.
	Certified bool
	// UARTPort is the UART port wired to its RS485, "" when the user picks
	// it.
	UARTPort string
	// GPIOSetup: its setup button and user LED are on the GPIO lines.
	GPIOSetup bool
	// ScreenSetup: its setup button comes through its screen (the board's
	// UserIO(true)), and setup mode runs only when the board hands it on.
	ScreenSetup bool
}

// Zone is the system's own time zone, once read.
type Zone interface {
	Get() (string, bool)
	Set(name string)
}

// Host is what the app hands the board's extension (StartBoard).
type Host struct {
	Client  *client.Client
	Version string
	DataDir string
	// Language is the app's language.
	Language func() string
	// Location is the app's time zone.
	Location func() *time.Location
	// MonthPeriod is the billing month around t.
	MonthPeriod func(t time.Time) (start, end time.Time)
	// History is the history store, nil while it is not open.
	History func() *history.Store
}

// Board is what a board's extension adds to the app: its screen, relays,
// LED and button. Each method is called once at start, except Update and
// Refresh.
type Board interface {
	// Update hands it every poll.
	Update(d solar.Data, commOK bool, inverter string)
	// Refresh is for a change it cannot see coming: the language.
	Refresh()
	// Relays switches the board's relays, for the relay plugin; nil
	// without them.
	Relays() RelaySwitch
	// UserIO is the board's user LED and setup button (lcd: the button as
	// its screen hands it on); nil without them.
	UserIO(lcd bool) setupmode.HAL
	// Setup hands it setup mode.
	Setup(f func() setupmode.Setup)
	// RelayStates hands it the relays' state, from the plugin that drives
	// them (pluginapi.RelayStateProvider).
	RelayStates(f func() []pluginapi.RelayState)
	// Prices hands it the day-ahead prices of the plugin that shares them
	// (pluginapi.PriceProvider).
	Prices(f func() (pluginapi.Prices, bool))
	// Forecast hands it the solar forecast of the plugin that shares one
	// (pluginapi.ForecastProvider).
	Forecast(f func() (pluginapi.Forecast, bool))
}

// RelaySwitch switches a HAL relay by device id.
type RelaySwitch interface {
	SetRelay(device string, on bool) error
}

// Plugin is a compiled-in plugin.
type Plugin interface {
	pluginapi.Hooks
	Info() pluginapi.Info
	RegisterRoutes(mux *http.ServeMux)
}

// InverterRestarter restarts the inverter (the dashboard's button).
type InverterRestarter interface {
	SupportsRestart() bool
	Restart() error
}

// ModbusAccess is the inverter's registers, for a plugin that reads or
// writes them.
type ModbusAccess interface {
	ReadRegisters(slaveID, fc byte, address, count uint16) ([]uint16, error)
	WriteRegister(slaveID byte, address, value uint16) error
	WriteRegisters(slaveID byte, address uint16, values []uint16) error
}

// PluginHost is what the app hands the plugins (Plugins).
type PluginHost struct {
	Client             *client.Client
	DataDir            string
	DeviceSerial       string
	GetKeys            func() []string
	GetModbus          func() (ModbusAccess, byte, bool)
	GetTimezone        func() string
	GetActiveProfileID func() string
	GetFaultCatalog    func() pluginapi.FaultCatalog
	Product            string
	HALRelays          RelaySwitch
	LogEvent           func(source string) func(pluginapi.LogEvent)
}
