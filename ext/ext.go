// Package ext is SolaStat's default extension: it adds nothing. The app
// asks it for the device's system features, a board's hardware and the
// plugins (solastat/internal/extapi); here the device is what the public
// SDK reads, there is no board hardware and there are no plugins.
//
// A build can replace this module with its own (a go.work replace of
// solastat/ext) offering the same three functions.
package ext

import (
	"context"
	"errors"

	"solastat/internal/extapi"
	"solastat/internal/setupmode"
	"solastat/internal/solar"

	"github.com/pedronsg/solastat-template/pkg/pluginapi"

	"github.com/OrbitOS-org/orbit-os-sdk-go/v26/client"
)

// Device is the device's system as the public SDK reads it.
func Device(c *client.Client) extapi.Device { return device{c} }

// StartBoard adds no board hardware.
func StartBoard(context.Context, extapi.Host) extapi.Board { return noBoard{} }

// Plugins are none.
func Plugins(extapi.PluginHost) ([]extapi.Plugin, extapi.InverterRestarter) { return nil, nil }

type device struct{ c *client.Client }

// OpenSetupAP is nil: setup mode opens its network with StartAP, no
// captive portal.
func (device) OpenSetupAP() func(ifname, ssid string, portal bool) error { return nil }

// Unit is the zero Unit: any board, the DIY edition.
func (device) Unit(string) extapi.Unit { return extapi.Unit{} }

// HostName is the device name, the host name on the public SDK's devices.
func (d device) HostName() (string, error) { return d.c.SystemManager.GetDeviceName() }

// NameSettable is false: the public SDK cannot set the device's name.
func (device) NameSettable() bool { return false }

func (device) SetName(string) error {
	return errors.New("the device's name can't be set in this build")
}

// WatchTimezone does nothing: the public SDK cannot read the system's time
// zone, so SolaStat's own setting is the app's.
func (device) WatchTimezone(context.Context, extapi.Zone) {}

func (device) SetTimezone(extapi.Zone, string) error {
	return errors.New("the system's time zone is not available in this build")
}

type noBoard struct{}

func (noBoard) Update(solar.Data, bool, string)            {}
func (noBoard) Refresh()                                   {}
func (noBoard) Relays() extapi.RelaySwitch                 { return nil }
func (noBoard) UserIO(bool) setupmode.HAL                  { return nil }
func (noBoard) Setup(func() setupmode.Setup)               {}
func (noBoard) RelayStates(func() []pluginapi.RelayState)  {}
func (noBoard) Prices(func() (pluginapi.Prices, bool))     {}
func (noBoard) Forecast(func() (pluginapi.Forecast, bool)) {}
