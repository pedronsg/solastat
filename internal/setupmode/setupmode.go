// Package setupmode runs the device's setup button, its user LED and
// the Wi-Fi setup (access point) mode.
//
// A long press on the button (5 s, released after the LED starts blinking
// fast) switches the device into AP mode: it opens a "SolaStat_2ce8"
// network, with no password, so a phone can join it and pick the home Wi-Fi in the setup wizard. The device also enters AP
// mode on its own when it has never joined a network, or when the home
// network has been unreachable for a while, and goes back to it by itself
// once it is reachable again, so a router that boots slower than the device
// after a power cut never leaves it stranded in AP mode.
//
// Everything runs inside the app (not the OrbitOS runtime). Where the HAL
// holds the LED and the button (Config.HAL), they go through it and the HAL
// tells the presses; otherwise the button is polled through GpioService (the
// SDK has no GPIO edge events).
package setupmode

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	wifisvc "github.com/OrbitOS-org/orbit-os-sdk-go/v26/api/wifi_service/v26"
	"github.com/OrbitOS-org/orbit-os-sdk-go/v26/client"
	"github.com/OrbitOS-org/orbit-os-sdk-go/v26/logger"

	"solastat/internal/fsutil"
)

const logTag = "setupmode"

// Timing of the automatic AP rules. Variables rather than constants so
// tests can shorten them.
var (
	pollActive        = 100 * time.Millisecond // button read while it is (or may be) pressed
	pollIdle          = 500 * time.Millisecond // button read at rest: each read is a gRPC round trip
	wifiCheckEvery    = 15 * time.Second       // Wi-Fi supervisor loop (each check is logged by the runtime)
	clientInfoEvery   = 5 * time.Minute        // refresh SSID/signal/IP this often while connected
	apAfterFirstBoot  = 60 * time.Second       // never joined a network: open AP after this
	apAfterDisconnect = 5 * time.Minute        // joined before, now unreachable: open AP after this
	apIdleTimeout     = 15 * time.Minute       // AP with nobody connected: go back to the home network
	apRetryEvery      = 3 * time.Minute        // automatic AP: look for the home network this often
	connectWait       = 30 * time.Second       // how long to wait for the Wi-Fi client to come up
	connectPoll       = 2 * time.Second
	apActivityGrace   = 2 * time.Minute // no AP retry within this long of the last client activity
	gpioErrorWarnAt   = 20              // consecutive failed button reads before treating the button as unavailable
	dirRecheckEvery   = time.Minute     // while the button pin isn't an input, check it again this often
	halButtonRetry    = time.Minute     // the HAL's button refused or gone: ask for it again this often
)

// GPIO is the part of the SDK's GpioManager this package uses.
type GPIO interface {
	SetDirection(pin *client.GpioPin, dir client.GpioDirection) error
	GetDirection(pin *client.GpioPin) (client.GpioDirection, error)
	GetLevel(pin *client.GpioPin) (client.GpioLevel, error)
	SetLevel(pin *client.GpioPin, level client.GpioLevel) error
}

// Press is a press of the button as the HAL classifies it.
type Press int

const (
	// ShortPress is a hold released before the HAL's long-press limit.
	ShortPress Press = iota + 1
	// LongPress comes once the hold reaches the limit (the button maybe
	// still down).
	LongPress
)

// HAL is the board's LED and button as its HAL announces them
// (orbit.device.led, orbit.device.button), on a board whose HAL holds their
// GPIO lines.
type HAL interface {
	SetLED(on bool) error
	// Presses delivers the button's presses until ctx is cancelled.
	Presses(ctx context.Context) (<-chan Press, error)
}

// WiFi is the part of the SDK's WiFiManager this package uses.
type WiFi interface {
	ListInterfaces() ([]*wifisvc.WiFiLinkProperties, error)
	IsConnected(ifname string) (bool, error)
	GetClientProperties(ifname string) (*wifisvc.ClientProperties, error)
	SetClientConfig(ifname, ssid, password, security string, dhcpEnable bool, ipv4Address, ipv4Gateway string, ipv4Dns []string) (bool, error)
	Connect(ifname string) (bool, error)
	StartAP(ifname, ssid, password, band string, channel int32) (bool, error)
	StopAP(ifname string) (bool, error)
	GetAPProperties(ifname string) (*wifisvc.APProperties, error)
	GetMode(ifname string) (wifisvc.WiFiMode, error)
	Scan(ifname string, forceRescan bool) ([]*wifisvc.ScannedNetwork, error)
}

// Config wires the service to the device.
type Config struct {
	GPIO GPIO
	WiFi WiFi

	// HAL, when set, drives the LED and reads the button instead of GPIO,
	// ButtonPin and LedPin.
	HAL HAL

	// ButtonPin reads LOW while pressed (the board has its own pull-up).
	ButtonPin *client.GpioPin
	// LedPin drives the user LED: HIGH = lit, or LOW = lit when LedActiveLow
	// is set (LED wired from 3.3 V to the pin).
	LedPin       *client.GpioPin
	LedActiveLow bool

	// Serial names the AP where DeviceHost can't (APSSID).
	Serial string
	// DeviceHost is the device's name on the network ("orbit-2ce8.local"),
	// what the setup wizard's address is told with; "" when the system
	// doesn't tell it, and the AP's address stands in. It names the AP too
	// (APSSID).
	DeviceHost string
	// JoinedMarkerPath is a file written once the device has joined a Wi-Fi
	// network, so "never configured" survives restarts.
	JoinedMarkerPath string
	// CommOK reports whether the inverter is answering (for the LED).
	CommOK func() bool
	// WiredUp, if set, reports whether the device is on a wired network
	// (Ethernet): the AP then never opens by itself, and one that
	// did closes, since the device can be reached without it. The button
	// still opens it.
	WiredUp func() bool
	// OpenAP, if set, opens the setup network (open, 2.4 GHz) instead of
	// WiFi.StartAP, with the captive portal when portal is true (API 26.2,
	// from the app's extension): a phone that joins is sent to the device's page by
	// itself. Asked for only off a wired network, whose clients the portal
	// would cut from the Internet.
	OpenAP func(ifname, ssid string, portal bool) error
	// OnEvent, if set, receives user-facing events for the activity log.
	OnEvent func(kind, text string)
}

// Network is a Wi-Fi network found by a scan.
type Network struct {
	SSID     string `json:"ssid"`
	Security string `json:"security"`
	Signal   int32  `json:"signal"`
	Band     string `json:"band,omitempty"`
}

// Status is what /api/wifi/status reports.
type Status struct {
	Available bool   `json:"available"`
	Interface string `json:"interface,omitempty"`
	Mode      string `json:"mode,omitempty"` // "ap" or "client"
	Connected bool   `json:"connected"`
	Wired     bool   `json:"wired"` // on a wired network (Config.WiredUp)
	SSID      string `json:"ssid,omitempty"`
	Signal    int32  `json:"signal,omitempty"`
	IP        string `json:"ip,omitempty"`
	APSSID    string `json:"ap_ssid,omitempty"`
	APIP      string `json:"ap_ip,omitempty"`
	APClients int32  `json:"ap_clients"`
	Busy      bool   `json:"busy"`
	LastError string `json:"last_error,omitempty"`
	// DeviceHost is the device's name on the network, for the address to go
	// back to once the phone is on the home network; "" when unknown.
	DeviceHost string `json:"device_host,omitempty"`
}

type requestKind int

const (
	reqToggleAP requestKind = iota
	reqStartAP
	reqStopAP
	reqConnect
)

type request struct {
	kind                     requestKind
	ssid, password, security string
}

// Service is the running setup-mode controller. A nil *Service is valid:
// its HTTP handlers then report that setup mode isn't available (boards
// without the setup button).
type Service struct {
	cfg    Config
	ifname string
	reqs   chan request
	now    func() time.Time

	mu                sync.Mutex
	apActive          bool
	apStarting        bool // AP requested, still scanning/starting: the LED already shows setup mode
	apAuto            bool // entered automatically (so it keeps looking for the home network)
	apSince           time.Time
	lastClientAt      time.Time
	lastRetry         time.Time
	disconnectedSince time.Time
	joined            bool // has joined a Wi-Fi network at least once
	connected         bool
	busy              bool
	clientSSID        string
	clientSignal      int32
	clientIP          string
	clientInfoAt      time.Time // when clientSSID/Signal/IP were last fetched
	apClients         int32
	apIP              string // the device's address on its own AP, once the runtime reports it
	lastError         string
	lastScan          []Network

	btn        buttonTracker
	ledMode    ledMode
	ledSince   time.Time
	ledLevel   bool
	ledKnown   bool
	gpioErrors int
	dirFault   bool      // the button pin isn't configured as an input (e.g. claimed by a PWM overlay)
	dirChecked time.Time // last time the pin direction was checked
	faultShown bool      // the "button unavailable" event was already logged
	ledFailed  bool      // the last LED write failed (logged once until it works again)
}

// New prepares the service; call Run to start it.
func New(cfg Config) *Service {
	s := &Service{
		cfg:    cfg,
		ifname: "wlan0",
		reqs:   make(chan request, 4),
		now:    time.Now,
	}
	if cfg.JoinedMarkerPath != "" {
		if _, err := os.Stat(cfg.JoinedMarkerPath); err == nil {
			s.joined = true
		}
	}
	return s
}

// Run starts the button/LED loop and the Wi-Fi supervisor and blocks until
// ctx is cancelled. Each loop recovers from its own panics, so a bad SDK
// response can never take the dashboard down with it.
func (s *Service) Run(ctx context.Context) {
	s.pickInterface()
	if s.cfg.HAL != nil {
		logger.Infof(logTag, "setup button and LED through the HAL, Wi-Fi interface %s, AP %q", s.ifname, s.apSSID())
	} else {
		s.checkButtonDirection(s.now())
		if err := s.cfg.GPIO.SetDirection(s.cfg.LedPin, client.GPIO_DIR_OUT); err != nil {
			logger.Errorf(logTag, "LED GPIO%d as output: %v", s.cfg.LedPin.Number, err)
		}
		logger.Infof(logTag, "setup button on GPIO%d, LED on GPIO%d, Wi-Fi interface %s, AP %q",
			s.cfg.ButtonPin.Number, s.cfg.LedPin.Number, s.ifname, s.apSSID())
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); s.buttonLoop(ctx) }()
	go func() { defer wg.Done(); s.superviseWiFi(ctx) }()
	if s.cfg.HAL != nil {
		wg.Add(1)
		go func() { defer wg.Done(); s.halPresses(ctx) }()
	}
	wg.Wait()
	_ = s.setLED(false)
}

// setLED lights (lit) or puts out the LED, through the HAL or its GPIO line.
func (s *Service) setLED(lit bool) error {
	if s.cfg.HAL != nil {
		return s.cfg.HAL.SetLED(lit)
	}
	return s.cfg.GPIO.SetLevel(s.cfg.LedPin, s.ledLevelFor(lit))
}

// halPresses hands the HAL's button presses to handleButton. A button the
// HAL won't give (none announced, busy with another app) is asked for again
// every halButtonRetry.
func (s *Service) halPresses(ctx context.Context) {
	failed := false
	for {
		ch, err := s.cfg.HAL.Presses(ctx)
		if err != nil {
			if !failed {
				logger.Errorf(logTag, "setup button: %v (trying again every %v)", err, halButtonRetry)
				failed = true
			}
		} else {
			if failed {
				logger.Infof(logTag, "setup button available again")
				failed = false
			}
			for p := range ch {
				ev, kind := evShortPress, "short"
				if p == LongPress {
					ev, kind = evLongPress, "long"
				}
				logger.Infof(logTag, "button: %s press", kind)
				s.safely(func() { s.handleButton(ev, s.now()) })
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(halButtonRetry):
		}
	}
}

// ledLevelFor is the pin level that turns the LED on (lit) or off, per the
// board's LED polarity (Config.LedActiveLow).
func (s *Service) ledLevelFor(lit bool) client.GpioLevel {
	if lit != s.cfg.LedActiveLow {
		return client.GPIO_LEVEL_HIGH
	}
	return client.GPIO_LEVEL_LOW
}

// buttonLoop reads the button and drives the LED. Every GPIO call is a
// gRPC round trip to the runtime, which is not free on the Pi Zero W's single
// core, so the loop sleeps as long as it can: the button is read every
// pollIdle at rest (a press lasts well over that) and every pollActive once
// it is down, and the LED is only touched when its pattern changes level.
func (s *Service) buttonLoop(ctx context.Context) {
	t := time.NewTimer(0)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.safely(s.buttonTick)
			t.Reset(s.nextTick(s.now()))
		}
	}
}

// nextTick is how long buttonLoop can sleep before it has to read the
// button again or change the LED.
func (s *Service) nextTick(now time.Time) time.Duration {
	d := pollIdle
	if s.btn.pressed || s.btn.count > 0 {
		d = pollActive
	}
	if edge := ledNextEdge(s.ledMode, now.Sub(s.ledSince)); edge > 0 && edge < d {
		d = edge
	}
	if d < 20*time.Millisecond {
		d = 20 * time.Millisecond
	}
	return d
}

func (s *Service) safely(fn func()) {
	defer func() {
		if r := recover(); r != nil {
			logger.Errorf(logTag, "recovered from panic: %v", r)
		}
	}()
	fn()
}

func (s *Service) pickInterface() {
	ifaces, err := s.cfg.WiFi.ListInterfaces()
	if err != nil {
		logger.Errorf(logTag, "list Wi-Fi interfaces: %v (using %s)", err, s.ifname)
		return
	}
	for _, l := range ifaces {
		if strings.HasPrefix(l.GetInterfaceName(), "wlan") {
			s.ifname = l.GetInterfaceName()
			return
		}
	}
	if len(ifaces) > 0 && ifaces[0].GetInterfaceName() != "" {
		s.ifname = ifaces[0].GetInterfaceName()
	}
}

// ── Button + LED ─────────────────────────────────────────────────────────────

// checkButtonDirection makes the button pin an input and confirms it took.
// If a device-tree overlay has claimed the pin (dtoverlay=pwm-2chan takes
// GPIO18 for PWM), reads return whatever the PWM block drives, which looks
// like random presses; the button is then ignored and reported instead of
// trusted.
func (s *Service) checkButtonDirection(now time.Time) {
	s.dirChecked = now
	if err := s.cfg.GPIO.SetDirection(s.cfg.ButtonPin, client.GPIO_DIR_IN); err != nil {
		logger.Errorf(logTag, "button GPIO%d as input: %v", s.cfg.ButtonPin.Number, err)
	}
	dir, err := s.cfg.GPIO.GetDirection(s.cfg.ButtonPin)
	fault := err != nil || dir != client.GPIO_DIR_IN
	if fault && !s.dirFault {
		logger.Errorf(logTag, "setup button GPIO%d is not an input (direction %v, err %v): ignoring it. If the board config has dtoverlay=pwm-2chan, that overlay claims GPIO18 for PWM; remove it.",
			s.cfg.ButtonPin.Number, dir, err)
	}
	if !fault && s.dirFault {
		logger.Infof(logTag, "setup button GPIO%d is an input again", s.cfg.ButtonPin.Number)
	}
	s.dirFault = fault
	s.reportButtonFault()
}

// buttonFault reports whether the setup button can't be trusted right now.
func (s *Service) buttonFault() bool {
	return s.dirFault || s.gpioErrors >= gpioErrorWarnAt
}

func (s *Service) reportButtonFault() {
	if s.buttonFault() && !s.faultShown {
		s.faultShown = true
		s.event("button_unavailable", fmt.Sprintf("Setup button unavailable: GPIO%d can't be read as an input (check the board's dtoverlay settings)", s.cfg.ButtonPin.Number))
	}
	if !s.buttonFault() {
		s.faultShown = false
	}
}

func (s *Service) buttonTick() {
	now := s.now()
	if s.cfg.HAL != nil { // the HAL tells the presses (halPresses): only the LED here
		s.updateLED(now)
		return
	}
	if s.dirFault {
		if now.Sub(s.dirChecked) >= dirRecheckEvery {
			s.checkButtonDirection(now)
		}
		if s.dirFault {
			s.updateLED(now)
			return
		}
	}
	level, err := s.cfg.GPIO.GetLevel(s.cfg.ButtonPin)
	if err != nil {
		s.gpioErrors++
		if s.gpioErrors == gpioErrorWarnAt {
			logger.Errorf(logTag, "cannot read the setup button on GPIO%d: %v. If the board config still has dtoverlay=pwm-2chan, that overlay claims GPIO18 for PWM and the button can't be read.",
				s.cfg.ButtonPin.Number, err)
			s.reportButtonFault()
		}
	} else {
		if s.gpioErrors >= gpioErrorWarnAt {
			logger.Infof(logTag, "setup button readable again")
		}
		s.gpioErrors = 0
		s.reportButtonFault()
		wasPressed := s.btn.pressed
		ev := s.btn.sample(level == client.GPIO_LEVEL_LOW, now)
		if s.btn.pressed != wasPressed {
			if s.btn.pressed {
				logger.Infof(logTag, "button pressed (GPIO%d level %d)", s.cfg.ButtonPin.Number, level)
			} else {
				logger.Infof(logTag, "button released after %v (GPIO%d level %d)", s.btn.lastHeld.Round(100*time.Millisecond), s.cfg.ButtonPin.Number, level)
			}
		}
		s.handleButton(ev, now)
	}
	s.updateLED(now)
}

func (s *Service) handleButton(ev buttonEvent, now time.Time) {
	switch ev {
	case evShortPress:
		// Like Sonoff: a short press leaves setup mode (back to the home
		// Wi-Fi); outside setup mode it does nothing.
		if s.isAPActive() {
			logger.Infof(logTag, "short press: leaving setup mode")
			if err := s.submit(request{kind: reqStopAP}); err != nil {
				logger.Errorf(logTag, "short press ignored: %v", err)
			}
		}
	case evLongReady:
		logger.Infof(logTag, "long press reached, release to toggle setup mode")
	case evLongPress:
		logger.Infof(logTag, "long press: toggling setup mode")
		entering := !s.isAPActive()
		if err := s.submit(request{kind: reqToggleAP}); err != nil {
			logger.Errorf(logTag, "long press ignored: %v", err)
		} else if entering {
			s.setAPStarting(true)
		}
	case evCancelled:
		logger.Infof(logTag, "button held too long, ignoring this press")
	}
}

func (s *Service) updateLED(now time.Time) {
	mode := s.baseLEDMode()
	switch {
	case s.btn.readyToRelease():
		mode = ledReleaseNow
	case s.btn.pressed && !s.btn.cancelled:
		mode = ledHeld // feedback that the press registered
	}
	if mode != s.ledMode {
		s.ledMode = mode
		s.ledSince = now
	}
	lit := ledLit(mode, now.Sub(s.ledSince))
	if s.ledKnown && lit == s.ledLevel {
		return
	}
	if err := s.setLED(lit); err != nil {
		if !s.ledFailed {
			logger.Errorf(logTag, "LED: %v", err)
			s.ledFailed = true
		}
		return
	}
	if s.ledFailed {
		logger.Infof(logTag, "LED working again")
		s.ledFailed = false
	}
	s.ledLevel = lit
	s.ledKnown = true
}

func (s *Service) baseLEDMode() ledMode {
	s.mu.Lock()
	ap, connected, starting := s.apActive, s.connected, s.apStarting
	s.mu.Unlock()
	switch {
	case ap, starting:
		return ledAP
	case !connected:
		return ledNoWiFi
	case s.cfg.CommOK != nil && !s.cfg.CommOK():
		return ledCommError
	}
	return ledOn
}

// ── Requests (button and HTTP) ───────────────────────────────────────────────

// ErrBusy is returned when a Wi-Fi change is already queued.
var ErrBusy = errors.New("a Wi-Fi change is already in progress")

func (s *Service) submit(r request) error {
	select {
	case s.reqs <- r:
		return nil
	default:
		return ErrBusy
	}
}

// StartAP asks for setup (AP) mode to be switched on.
func (s *Service) StartAP() error {
	if err := s.submit(request{kind: reqStartAP}); err != nil {
		return err
	}
	s.setAPStarting(true)
	return nil
}

// StopAP asks for setup mode to be switched off (back to the home network).
func (s *Service) StopAP() error { return s.submit(request{kind: reqStopAP}) }

// Connect saves the home network and switches to it. It returns at once:
// if the phone is on the device's AP, it loses that connection as soon as the
// device switches, so the result is read later from Status (LastError).
func (s *Service) Connect(ssid, password, security string) error {
	if strings.TrimSpace(ssid) == "" {
		return errors.New("ssid is required")
	}
	if security == "" {
		security = "wpa2"
		if password == "" {
			security = "none"
		}
	}
	return s.submit(request{kind: reqConnect, ssid: ssid, password: password, security: security})
}

// ── Wi-Fi supervisor ─────────────────────────────────────────────────────────

func (s *Service) superviseWiFi(ctx context.Context) {
	t := time.NewTicker(wifiCheckEvery)
	defer t.Stop()
	s.safely(func() { s.wifiTick(ctx) })
	for {
		select {
		case <-ctx.Done():
			return
		case r := <-s.reqs:
			s.safely(func() { s.handleRequest(ctx, r) })
		case <-t.C:
			s.safely(func() { s.wifiTick(ctx) })
		}
	}
}

func (s *Service) handleRequest(ctx context.Context, r request) {
	s.setBusy(true)
	defer s.setBusy(false)
	// Entering setup mode takes a few seconds (Wi-Fi scan, then starting the
	// AP); apStarting shows it on the LED right away and is cleared once the
	// request is done, whether the AP came up or not.
	defer s.setAPStarting(false)
	switch r.kind {
	case reqToggleAP:
		if s.isAPActive() {
			s.leaveAP(ctx, "button")
		} else {
			s.enterAP(false, "button")
		}
	case reqStartAP:
		if !s.isAPActive() {
			s.enterAP(false, "request")
		}
	case reqStopAP:
		if s.isAPActive() {
			s.leaveAP(ctx, "request")
		}
	case reqConnect:
		s.connectTo(ctx, r)
	}
}

func (s *Service) wifiTick(ctx context.Context) {
	now := s.now()
	if s.isAPActive() {
		s.apTick(ctx, now)
		return
	}

	// The AP may also have been started outside SolaStat (e.g. from the
	// Orbit OS Settings app): adopt it so the LED and the rules follow.
	if mode, err := s.cfg.WiFi.GetMode(s.ifname); err == nil && mode == wifisvc.WiFiMode_WIFI_MODE_AP {
		s.adoptExternalAP(now)
		return
	}

	connected, err := s.cfg.WiFi.IsConnected(s.ifname)
	if err != nil {
		logger.Errorf(logTag, "Wi-Fi status: %v", err)
		return
	}
	if connected {
		s.markConnected()
		return
	}
	if s.wiredUp() {
		// Reachable over the cable: no Wi-Fi is nothing to fix.
		s.mu.Lock()
		s.connected = false
		s.disconnectedSince = time.Time{}
		s.mu.Unlock()
		return
	}

	s.mu.Lock()
	s.connected = false
	if s.disconnectedSince.IsZero() {
		s.disconnectedSince = now
	}
	limit := apAfterDisconnect
	if !s.joined {
		limit = apAfterFirstBoot
	}
	due := now.Sub(s.disconnectedSince) >= limit
	s.mu.Unlock()
	if due {
		s.enterAP(true, "no Wi-Fi")
	}
}

func (s *Service) apTick(ctx context.Context, now time.Time) {
	props, err := s.cfg.WiFi.GetAPProperties(s.ifname)
	if (err != nil && strings.Contains(err.Error(), "not in AP mode")) || (err == nil && !props.GetActive()) {
		// The AP is gone, but not because of us: typically the Wi-Fi was set
		// up from the Orbit OS Settings app, which stops the AP and joins the
		// home network. Double-check the mode, then follow it.
		if mode, merr := s.cfg.WiFi.GetMode(s.ifname); merr == nil && mode != wifisvc.WiFiMode_WIFI_MODE_AP && mode != wifisvc.WiFiMode_WIFI_MODE_AP_CLIENT {
			s.dropExternalAP()
			return
		}
	}
	if err == nil {
		s.mu.Lock()
		s.apClients = props.GetConnectedClients()
		if ip := stripPrefix(props.GetIpv4Address()); ip != "" {
			s.apIP = ip
		}
		if s.apClients > 0 {
			s.lastClientAt = now
		}
		s.mu.Unlock()
	}

	s.mu.Lock()
	joined, auto, clients := s.joined, s.apAuto, s.apClients
	idleSince := s.apSince
	if s.lastClientAt.After(idleSince) {
		idleSince = s.lastClientAt
	}
	retryDue := now.Sub(s.lastRetry) >= apRetryEvery
	s.mu.Unlock()

	// Never pull the AP from under a phone that is using it (by the
	// runtime's count or by recent requests to the web UI).
	inUse := clients > 0 || now.Sub(idleSince) < apActivityGrace
	if auto && !inUse && s.wiredUp() {
		s.closeAP("on a wired network")
		return
	}
	// Nothing to go back to until a home network has been set up.
	if !joined || inUse {
		return
	}
	switch {
	case now.Sub(idleSince) >= apIdleTimeout:
		s.leaveAP(ctx, "idle")
	case auto && retryDue:
		s.mu.Lock()
		s.lastRetry = now
		s.mu.Unlock()
		logger.Infof(logTag, "looking for the home Wi-Fi again")
		if !s.switchToClient(ctx) {
			s.reopenAP()
		}
	}
}

// adoptExternalAP takes over an AP that was started outside SolaStat.
func (s *Service) adoptExternalAP(now time.Time) {
	s.mu.Lock()
	s.apActive = true
	s.apAuto = false
	s.apSince = now
	s.lastClientAt = time.Time{}
	s.lastRetry = now
	s.connected = false
	s.mu.Unlock()
	logger.Infof(logTag, "Wi-Fi is in AP mode (started outside SolaStat): following it")
}

// dropExternalAP forgets an AP that was stopped outside SolaStat and goes
// back to watching the Wi-Fi client.
func (s *Service) dropExternalAP() {
	s.mu.Lock()
	s.apActive = false
	s.apAuto = false
	s.apClients = 0
	s.apIP = ""
	s.disconnectedSince = time.Time{}
	s.mu.Unlock()
	logger.Infof(logTag, "AP was stopped outside SolaStat (e.g. Wi-Fi set up in Orbit OS Settings): back to client mode")
	if ok, err := s.cfg.WiFi.IsConnected(s.ifname); err == nil && ok {
		s.markConnected()
		s.event("setup_mode_off", "Setup mode off: connected to the home Wi-Fi")
	}
}

// closeAP stops the AP without looking for the home Wi-Fi: the device is
// reachable another way (WiredUp).
func (s *Service) closeAP(reason string) {
	logger.Infof(logTag, "leaving setup mode (%s)", reason)
	if _, err := s.cfg.WiFi.StopAP(s.ifname); err != nil {
		logger.Errorf(logTag, "stop AP: %v", err)
	}
	s.mu.Lock()
	s.apActive = false
	s.apAuto = false
	s.apClients = 0
	s.apIP = ""
	s.disconnectedSince = time.Time{}
	s.mu.Unlock()
	s.event("setup_mode_off", "Setup mode off: the device is on a wired network")
}

// enterAP scans first (the single radio can't reliably scan once it is an
// access point), then opens the setup network.
func (s *Service) enterAP(auto bool, reason string) {
	if nets, err := s.scan(true); err == nil && len(nets) > 0 {
		s.mu.Lock()
		s.lastScan = nets
		s.mu.Unlock()
	}
	ssid := s.apSSID()
	if err := s.openAP(ssid); err != nil {
		logger.Errorf(logTag, "start AP: %v", err)
		s.setError("could not start setup mode: " + err.Error())
		return
	}
	now := s.now()
	s.mu.Lock()
	s.apActive = true
	s.apAuto = auto
	s.apSince = now
	s.lastClientAt = time.Time{}
	s.lastRetry = now
	s.connected = false
	s.apClients = 0
	s.apIP = ""
	s.mu.Unlock()
	// The address the setup page is reached at, for the screen's code; the
	// supervisor keeps it up to date if the runtime only has it later.
	if props, err := s.cfg.WiFi.GetAPProperties(s.ifname); err == nil {
		s.mu.Lock()
		s.apIP = stripPrefix(props.GetIpv4Address())
		s.mu.Unlock()
	}
	logger.Infof(logTag, "setup mode on (%s): AP %q", reason, ssid)
	s.event("setup_mode_on", "Setup mode on: join the Wi-Fi network \""+ssid+"\" to configure the device")
}

// leaveAP closes the setup network and goes back to the home Wi-Fi. If
// that fails and the device has nowhere else to go, the AP is reopened.
func (s *Service) leaveAP(ctx context.Context, reason string) {
	logger.Infof(logTag, "leaving setup mode (%s)", reason)
	if s.switchToClient(ctx) {
		s.event("setup_mode_off", "Setup mode off: connected to the home Wi-Fi")
		return
	}
	s.reopenAP()
}

// switchToClient stops the AP (if on) and waits for the Wi-Fi client to
// come up. It reports whether the device is now connected.
func (s *Service) switchToClient(ctx context.Context) bool {
	if s.isAPActive() {
		if _, err := s.cfg.WiFi.StopAP(s.ifname); err != nil {
			logger.Errorf(logTag, "stop AP: %v", err)
		}
		s.mu.Lock()
		s.apActive = false
		s.apIP = ""
		s.mu.Unlock()
	}
	if _, err := s.cfg.WiFi.Connect(s.ifname); err != nil {
		logger.Errorf(logTag, "Wi-Fi connect: %v", err)
	}
	if s.waitConnected(ctx) {
		s.markConnected()
		return true
	}
	return false
}

func (s *Service) reopenAP() {
	s.mu.Lock()
	auto := s.apAuto
	s.mu.Unlock()
	s.enterAP(auto, "home Wi-Fi not reachable")
}

func (s *Service) connectTo(ctx context.Context, r request) {
	// Give the HTTP response time to reach the phone before the AP drops.
	select {
	case <-ctx.Done():
		return
	case <-time.After(time.Second):
	}
	s.setError("")
	if _, err := s.cfg.WiFi.SetClientConfig(s.ifname, r.ssid, r.password, r.security, true, "", "", nil); err != nil {
		logger.Errorf(logTag, "save Wi-Fi %q: %v", r.ssid, err)
		s.setError("could not save the Wi-Fi settings: " + err.Error())
		return
	}
	wasAP := s.isAPActive()
	if s.switchToClient(ctx) {
		logger.Infof(logTag, "joined Wi-Fi %q", r.ssid)
		s.event("wifi_joined", "Connected to the Wi-Fi network \""+r.ssid+"\"")
		return
	}
	s.setError("could not connect to \"" + r.ssid + "\": check the password and that the network is 2.4 GHz")
	if wasAP || !s.joinedOnce() {
		s.mu.Lock()
		s.apAuto = false
		s.mu.Unlock()
		s.enterAP(false, "connection failed")
	}
}

func (s *Service) waitConnected(ctx context.Context) bool {
	deadline := time.Now().Add(connectWait)
	for {
		if ok, err := s.cfg.WiFi.IsConnected(s.ifname); err == nil && ok {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(connectPoll):
		}
	}
}

func (s *Service) markConnected() {
	// The SSID/signal/IP only change on a (re)connection, so they're fetched
	// then and refreshed rarely, instead of on every supervisor tick.
	s.mu.Lock()
	refresh := !s.connected || s.now().Sub(s.clientInfoAt) >= clientInfoEvery
	s.mu.Unlock()
	var props *wifisvc.ClientProperties
	if refresh {
		props, _ = s.cfg.WiFi.GetClientProperties(s.ifname)
	}
	s.mu.Lock()
	first := !s.joined
	s.connected = true
	s.joined = true
	s.disconnectedSince = time.Time{}
	if props != nil {
		s.clientInfoAt = s.now()
		s.clientSSID = props.GetSsid()
		s.clientSignal = props.GetSignalStrength()
		s.clientIP = stripPrefix(props.GetIpv4Address())
	}
	s.mu.Unlock()
	if first && s.cfg.JoinedMarkerPath != "" {
		if err := fsutil.WriteFileAtomic(s.cfg.JoinedMarkerPath, []byte("1\n"), 0o644); err != nil {
			logger.Errorf(logTag, "write %s: %v", s.cfg.JoinedMarkerPath, err)
		}
	}
}

// NoteActivity records that someone is using the web UI right now. While
// the AP is open this counts as a connected client, so the AP is never
// closed (or dropped to look for the home Wi-Fi) under someone who is
// configuring the device. The runtime's own client count relies on `iw` and
// reads 0 when that tool is missing, so it can't be trusted on its own.
func (s *Service) NoteActivity() {
	if s == nil {
		return
	}
	now := s.now()
	s.mu.Lock()
	if s.apActive {
		s.lastClientAt = now
	}
	s.mu.Unlock()
}

// Middleware wraps the app's HTTP handler so every request counts as
// activity (see NoteActivity).
func (s *Service) Middleware(next http.Handler) http.Handler {
	if s == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.NoteActivity()
		next.ServeHTTP(w, r)
	})
}

// ── Scanning and status ──────────────────────────────────────────────────────

// Networks returns the networks around the device, strongest first. While in
// AP mode a live scan may not be possible, so the list taken just before
// the AP opened is used as a fallback. force asks for a new scan rather
// than the system's recent one.
func (s *Service) Networks(force bool) []Network {
	if nets, err := s.scan(force); err == nil && len(nets) > 0 {
		s.mu.Lock()
		s.lastScan = nets
		s.mu.Unlock()
		return nets
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Network(nil), s.lastScan...)
}

// scan lists the networks around, the device's own setup network left out.
func (s *Service) scan(force bool) ([]Network, error) {
	raw, err := s.cfg.WiFi.Scan(s.ifname, force)
	if err != nil {
		return nil, err
	}
	own := map[string]bool{s.apSSID(): true}
	if s.isAPActive() {
		// An AP started outside SolaStat goes by its own name.
		if props, err := s.cfg.WiFi.GetAPProperties(s.ifname); err == nil {
			own[props.GetSsid()] = true
		}
	}
	best := map[string]Network{}
	for _, n := range raw {
		ssid := strings.TrimSpace(n.GetSsid())
		if ssid == "" || own[ssid] || n.GetHidden() {
			continue
		}
		if cur, ok := best[ssid]; ok && cur.Signal >= n.GetSignalStrength() {
			continue
		}
		best[ssid] = Network{SSID: ssid, Security: n.GetSecurity(), Signal: n.GetSignalStrength(), Band: n.GetBand()}
	}
	out := make([]Network, 0, len(best))
	for _, n := range best {
		out = append(out, n)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Signal != out[j].Signal {
			return out[i].Signal > out[j].Signal
		}
		return out[i].SSID < out[j].SSID
	})
	return out, nil
}

// Status reports the current Wi-Fi / setup-mode state.
func (s *Service) Status() Status {
	if s == nil {
		return Status{Available: false}
	}
	wired := s.wiredUp() // a call to the runtime: outside the lock
	s.mu.Lock()
	defer s.mu.Unlock()
	st := Status{
		Available:  true,
		Interface:  s.ifname,
		Mode:       "client",
		Connected:  s.connected,
		Wired:      wired,
		Busy:       s.busy,
		LastError:  s.lastError,
		DeviceHost: s.cfg.DeviceHost,
	}
	if s.apActive {
		st.Mode = "ap"
		st.APSSID = s.apSSID()
		st.APIP = s.apIP
		st.APClients = s.apClients
	}
	if s.connected {
		st.SSID, st.Signal, st.IP = s.clientSSID, s.clientSignal, s.clientIP
	}
	return st
}

// Setup is setup mode as the device's own screen shows it (an LCD):
// unlike Status it carries the AP's password, so it is never served over
// the API.
type Setup struct {
	// Active is the AP up; Starting asked for, still scanning or starting.
	Active, Starting bool
	SSID             string
	// Host is what a phone on the AP reaches the device at, its name (""
	// until the runtime reports the AP's address).
	Host string
}

// Setup reports setup mode for the device's screen; zero on a nil Service.
func (s *Service) Setup() Setup {
	if s == nil {
		return Setup{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	st := Setup{Active: s.apActive, Starting: s.apStarting && !s.apActive}
	if st.Active || st.Starting {
		st.SSID = s.apSSID()
	}
	if st.Active {
		if s.apIP != "" {
			st.Host = s.cfg.DeviceHost
			if st.Host == "" {
				st.Host = s.apIP
			}
		}
	}
	return st
}

// ── Small helpers ────────────────────────────────────────────────────────────

// openAP opens the setup network: through Config.OpenAP when set, with the
// captive portal off a wired network, else the SDK's plain StartAP.
func (s *Service) openAP(ssid string) error {
	if s.cfg.OpenAP != nil {
		return s.cfg.OpenAP(s.ifname, ssid, !s.wiredUp())
	}
	_, err := s.cfg.WiFi.StartAP(s.ifname, ssid, "", "2.4GHz", 0)
	return err
}

// apSSID is the setup network's name on this device.
func (s *Service) apSSID() string { return APSSID(s.cfg.DeviceHost, s.cfg.Serial) }

// wiredUp reports Config.WiredUp, false when unset.
func (s *Service) wiredUp() bool {
	return s.cfg.WiredUp != nil && s.cfg.WiredUp()
}

// stripPrefix drops the CIDR prefix the runtime gives addresses with
// ("192.168.4.1/24" is "192.168.4.1").
func stripPrefix(addr string) string {
	ip, _, _ := strings.Cut(addr, "/")
	return ip
}

func (s *Service) isAPActive() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.apActive
}

func (s *Service) joinedOnce() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.joined
}

func (s *Service) setAPStarting(b bool) {
	s.mu.Lock()
	s.apStarting = b
	s.mu.Unlock()
}

func (s *Service) setBusy(b bool) {
	s.mu.Lock()
	s.busy = b
	s.mu.Unlock()
}

func (s *Service) setError(msg string) {
	s.mu.Lock()
	s.lastError = msg
	s.mu.Unlock()
}

func (s *Service) event(kind, text string) {
	if s.cfg.OnEvent != nil {
		s.cfg.OnEvent(kind, text)
	}
}
