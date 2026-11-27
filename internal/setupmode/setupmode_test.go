package setupmode

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	wifisvc "github.com/OrbitOS-org/orbit-os-sdk-go/v26/api/wifi_service/v26"
	"github.com/OrbitOS-org/orbit-os-sdk-go/v26/client"
)

// ── Fakes ────────────────────────────────────────────────────────────────────

type fakeGPIO struct {
	mu       sync.Mutex
	pressed  bool
	readErr  error
	notInput bool // the button pin reports a non-input direction (e.g. claimed by PWM)
	led      client.GpioLevel
	sets     int
}

func (g *fakeGPIO) SetDirection(*client.GpioPin, client.GpioDirection) error { return nil }
func (g *fakeGPIO) GetDirection(*client.GpioPin) (client.GpioDirection, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.notInput {
		return client.GPIO_DIR_OUT, nil
	}
	return client.GPIO_DIR_IN, nil
}
func (g *fakeGPIO) GetLevel(*client.GpioPin) (client.GpioLevel, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.readErr != nil {
		return client.GPIO_LEVEL_LOW, g.readErr
	}
	if g.pressed {
		return client.GPIO_LEVEL_LOW, nil
	}
	return client.GPIO_LEVEL_HIGH, nil
}
func (g *fakeGPIO) SetLevel(_ *client.GpioPin, l client.GpioLevel) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.led = l
	g.sets++
	return nil
}

type fakeWiFi struct {
	mu          sync.Mutex
	connected   bool
	canConnect  bool // Connect() brings the client up
	ap          bool
	apClients   int32
	savedSSID   string
	startAPs    int
	stopAPs     int
	connects    int
	scanResults []*wifisvc.ScannedNetwork
	apPassword  string
}

func (f *fakeWiFi) ListInterfaces() ([]*wifisvc.WiFiLinkProperties, error) {
	return []*wifisvc.WiFiLinkProperties{{InterfaceName: "wlan0"}}, nil
}
func (f *fakeWiFi) IsConnected(string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.connected && !f.ap, nil
}
func (f *fakeWiFi) GetClientProperties(string) (*wifisvc.ClientProperties, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return &wifisvc.ClientProperties{Ssid: f.savedSSID, SignalStrength: -55, Ipv4Address: "192.168.1.50/24"}, nil
}
func (f *fakeWiFi) SetClientConfig(_ string, ssid, _, _ string, _ bool, _, _ string, _ []string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.savedSSID = ssid
	return true, nil
}
func (f *fakeWiFi) Connect(string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.connects++
	f.connected = f.canConnect
	return f.canConnect, nil
}
// markAP is the AP up, as an OpenAP of the tests leaves it.
func (f *fakeWiFi) markAP() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ap = true
	return nil
}

func (f *fakeWiFi) StartAP(_, _, password, _ string, _ int32) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ap = true
	f.apPassword = password
	f.startAPs++
	return true, nil
}
func (f *fakeWiFi) StopAP(string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ap = false
	f.stopAPs++
	return true, nil
}
func (f *fakeWiFi) GetAPProperties(string) (*wifisvc.APProperties, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.ap {
		return nil, errors.New("interface 'wlan0' is not in AP mode")
	}
	return &wifisvc.APProperties{Active: true, ConnectedClients: f.apClients, Ipv4Address: "192.168.4.1/24"}, nil
}
func (f *fakeWiFi) GetMode(string) (wifisvc.WiFiMode, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case f.ap:
		return wifisvc.WiFiMode_WIFI_MODE_AP, nil
	case f.connected:
		return wifisvc.WiFiMode_WIFI_MODE_CLIENT, nil
	}
	return wifisvc.WiFiMode_WIFI_MODE_UNKNOWN, nil
}
func (f *fakeWiFi) Scan(string, bool) ([]*wifisvc.ScannedNetwork, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.scanResults, nil
}

func fastTimings(t *testing.T) {
	t.Helper()
	oldWait, oldPoll := connectWait, connectPoll
	connectWait, connectPoll = 40*time.Millisecond, 5*time.Millisecond
	t.Cleanup(func() { connectWait, connectPoll = oldWait, oldPoll })
}

func newTestService(t *testing.T, w *fakeWiFi, joined bool) (*Service, *time.Time) {
	t.Helper()
	fastTimings(t)
	marker := filepath.Join(t.TempDir(), "wifi_joined.cfg")
	if joined {
		if err := os.WriteFile(marker, []byte("1\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	s := New(Config{
		GPIO:             &fakeGPIO{},
		WiFi:             w,
		ButtonPin:        &client.GpioPin{Number: 18},
		LedPin:           &client.GpioPin{Number: 23},
		Serial:           "NAXIM-SM01-000002",
		JoinedMarkerPath: marker,
	})
	now := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	return s, &now
}

// ── Button ───────────────────────────────────────────────────────────────────

// press holds the button for d, sampling every 100 ms, and returns every
// event produced, release included.
func press(b *buttonTracker, start time.Time, d time.Duration) []buttonEvent {
	var evs []buttonEvent
	t := start
	for ; t.Sub(start) <= d; t = t.Add(100 * time.Millisecond) {
		if ev := b.sample(true, t); ev != evNone {
			evs = append(evs, ev)
		}
	}
	for i := 0; i < debounceSamples; i++ {
		if ev := b.sample(false, t); ev != evNone {
			evs = append(evs, ev)
		}
		t = t.Add(100 * time.Millisecond)
	}
	return evs
}

func TestButtonShortPress(t *testing.T) {
	var b buttonTracker
	got := press(&b, time.Unix(0, 0), 300*time.Millisecond)
	if len(got) != 1 || got[0] != evShortPress {
		t.Fatalf("events = %v, want [short press]", got)
	}
}

func TestButtonLongPress(t *testing.T) {
	var b buttonTracker
	got := press(&b, time.Unix(0, 0), 6*time.Second)
	if len(got) != 2 || got[0] != evLongReady || got[1] != evLongPress {
		t.Fatalf("events = %v, want [long ready, long press]", got)
	}
}

func TestButtonHeldTooLongIsCancelled(t *testing.T) {
	var b buttonTracker
	got := press(&b, time.Unix(0, 0), 20*time.Second)
	if len(got) != 2 || got[0] != evLongReady || got[1] != evCancelled {
		t.Fatalf("events = %v, want [long ready, cancelled] and nothing on release", got)
	}
}

func TestButtonBetweenShortAndLongDoesNothing(t *testing.T) {
	var b buttonTracker
	if got := press(&b, time.Unix(0, 0), 2*time.Second); len(got) != 0 {
		t.Fatalf("events = %v, want none", got)
	}
}

func TestButtonIgnoresSingleSampleGlitch(t *testing.T) {
	var b buttonTracker
	t0 := time.Unix(0, 0)
	if ev := b.sample(true, t0); ev != evNone {
		t.Fatalf("glitch produced %v", ev)
	}
	if ev := b.sample(false, t0.Add(100*time.Millisecond)); ev != evNone {
		t.Fatalf("glitch produced %v", ev)
	}
	if b.pressed {
		t.Fatal("a single low sample should not count as a press")
	}
}

// ── LED ──────────────────────────────────────────────────────────────────────

func TestLEDPatterns(t *testing.T) {
	cases := []struct {
		mode    ledMode
		elapsed time.Duration
		want    bool
	}{
		{ledOff, time.Second, false},
		{ledOn, time.Second, true},
		{ledHeld, time.Second, true},
		{ledAP, 500 * time.Millisecond, true},   // 1 s on
		{ledAP, 1500 * time.Millisecond, false}, // 1 s off
		{ledAP, 2500 * time.Millisecond, true},
		{ledNoWiFi, 100 * time.Millisecond, true}, // one flash...
		{ledNoWiFi, time.Second, false},           // ...then a long pause
		{ledNoWiFi, 3100 * time.Millisecond, true},
		{ledCommError, 100 * time.Millisecond, true}, // first flash
		{ledCommError, 350 * time.Millisecond, false},
		{ledCommError, 600 * time.Millisecond, true}, // second flash
		{ledCommError, 1500 * time.Millisecond, false},
	}
	for _, c := range cases {
		if got := ledLit(c.mode, c.elapsed); got != c.want {
			t.Errorf("ledLit(%d, %v) = %v, want %v", c.mode, c.elapsed, got, c.want)
		}
	}
}

// ── Credentials ──────────────────────────────────────────────────────────────

func TestAPSSID(t *testing.T) {
	for _, c := range []struct{ host, serial, want string }{
		{"orbit-2ce8.local", "NAXIM-SM01-000002", "SolaStat_2ce8"},
		{"raspberrypi.local", "NAXIM-SM01-000002", "SolaStat_0002"},
		{"", "NAXIM-SM01-000002", "SolaStat_0002"},
		{"", "", "SolaStat"},
	} {
		if got := APSSID(c.host, c.serial); got != c.want {
			t.Errorf("APSSID(%q, %q) = %q, want %q", c.host, c.serial, got, c.want)
		}
	}
}

// ── Wi-Fi supervisor ─────────────────────────────────────────────────────────

func TestFirstBootOpensAPAfterGrace(t *testing.T) {
	w := &fakeWiFi{}
	s, now := newTestService(t, w, false)
	ctx := context.Background()

	s.wifiTick(ctx)
	if s.isAPActive() {
		t.Fatal("AP opened before the grace period")
	}
	*now = now.Add(apAfterFirstBoot)
	s.wifiTick(ctx)
	if !s.isAPActive() || w.startAPs != 1 {
		t.Fatalf("AP should open after %v on first boot (active=%v, starts=%d)", apAfterFirstBoot, s.isAPActive(), w.startAPs)
	}
	if st := s.Status(); st.Mode != "ap" || st.APSSID != "SolaStat_0002" {
		t.Fatalf("status = %+v", st)
	}
}

func TestJoinedHubWaitsLongerBeforeAP(t *testing.T) {
	w := &fakeWiFi{}
	s, now := newTestService(t, w, true)
	ctx := context.Background()

	s.wifiTick(ctx)
	*now = now.Add(apAfterFirstBoot)
	s.wifiTick(ctx)
	if s.isAPActive() {
		t.Fatal("a device that has joined before should wait longer than the first-boot grace")
	}
	*now = now.Add(apAfterDisconnect)
	s.wifiTick(ctx)
	if !s.isAPActive() {
		t.Fatal("AP should open once the home Wi-Fi has been gone long enough")
	}
}

func TestConnectedHubStaysOnClient(t *testing.T) {
	w := &fakeWiFi{connected: true}
	s, now := newTestService(t, w, false)
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		s.wifiTick(ctx)
		*now = now.Add(apAfterDisconnect)
	}
	if s.isAPActive() {
		t.Fatal("AP opened while connected")
	}
	if _, err := os.Stat(s.cfg.JoinedMarkerPath); err != nil {
		t.Fatalf("joined marker not written: %v", err)
	}
	if st := s.Status(); !st.Connected || st.IP != "192.168.1.50" {
		t.Fatalf("status = %+v", st)
	}
}

func TestAutomaticAPReturnsHomeWhenReachable(t *testing.T) {
	w := &fakeWiFi{}
	s, now := newTestService(t, w, true)
	ctx := context.Background()
	s.enterAP(true, "test")

	w.canConnect = true
	*now = now.Add(apRetryEvery)
	s.wifiTick(ctx)
	if s.isAPActive() {
		t.Fatal("automatic AP should go back to the home Wi-Fi once it is reachable")
	}
	if !s.Status().Connected {
		t.Fatal("should be connected")
	}
}

func TestAPStaysOpenWhileSomeoneIsConnected(t *testing.T) {
	w := &fakeWiFi{canConnect: true, apClients: 1}
	s, now := newTestService(t, w, true)
	ctx := context.Background()
	s.enterAP(true, "test")

	*now = now.Add(apIdleTimeout + apRetryEvery)
	s.wifiTick(ctx)
	if !s.isAPActive() || w.stopAPs != 0 {
		t.Fatal("AP must not be closed while a phone is connected to it")
	}
}

func TestManualAPClosesAfterIdleTimeout(t *testing.T) {
	w := &fakeWiFi{canConnect: true}
	s, now := newTestService(t, w, true)
	ctx := context.Background()
	s.enterAP(false, "button")

	*now = now.Add(apRetryEvery)
	s.wifiTick(ctx)
	if !s.isAPActive() {
		t.Fatal("a manually opened AP should not look for the home Wi-Fi before the idle timeout")
	}
	*now = now.Add(apIdleTimeout)
	s.wifiTick(ctx)
	if s.isAPActive() {
		t.Fatal("AP should close after the idle timeout")
	}
}

func TestUnconfiguredHubKeepsAPOpen(t *testing.T) {
	w := &fakeWiFi{}
	s, now := newTestService(t, w, false)
	ctx := context.Background()
	s.enterAP(true, "test")
	*now = now.Add(apIdleTimeout * 3)
	s.wifiTick(ctx)
	if !s.isAPActive() {
		t.Fatal("with no home network set up there is nothing to go back to")
	}
}

func TestConnectRequestJoinsNetwork(t *testing.T) {
	w := &fakeWiFi{canConnect: true}
	s, _ := newTestService(t, w, false)
	ctx := context.Background()
	s.enterAP(false, "button")

	s.connectTo(ctx, request{kind: reqConnect, ssid: "Casa", password: "secret123", security: "wpa2"})
	if s.isAPActive() {
		t.Fatal("AP should close after joining the home Wi-Fi")
	}
	st := s.Status()
	if !st.Connected || st.SSID != "Casa" || st.LastError != "" {
		t.Fatalf("status = %+v", st)
	}
}

func TestConnectRequestFailureReopensAP(t *testing.T) {
	w := &fakeWiFi{canConnect: false}
	s, _ := newTestService(t, w, false)
	ctx := context.Background()
	s.enterAP(false, "button")

	s.connectTo(ctx, request{kind: reqConnect, ssid: "Casa", password: "wrong", security: "wpa2"})
	if !s.isAPActive() {
		t.Fatal("AP should come back so the phone can try again")
	}
	if s.Status().LastError == "" {
		t.Fatal("the failure should be reported in the status")
	}
}

func TestLongPressTogglesAP(t *testing.T) {
	w := &fakeWiFi{canConnect: true, connected: true}
	s, _ := newTestService(t, w, true)
	ctx := context.Background()

	s.handleButton(evLongPress, s.now())
	s.handleRequest(ctx, <-s.reqs)
	if !s.isAPActive() {
		t.Fatal("long press should open the AP")
	}
	s.handleButton(evLongPress, s.now())
	s.handleRequest(ctx, <-s.reqs)
	if s.isAPActive() {
		t.Fatal("a second long press should close it again")
	}
}

func TestLEDFollowsState(t *testing.T) {
	w := &fakeWiFi{connected: true}
	s, now := newTestService(t, w, true)
	g := s.cfg.GPIO.(*fakeGPIO)
	ok := true
	s.cfg.CommOK = func() bool { return ok }

	s.wifiTick(context.Background())
	s.updateLED(*now)
	if g.led != client.GPIO_LEVEL_HIGH {
		t.Fatal("LED should be steady on when everything is fine")
	}
	ok = false
	if s.baseLEDMode() != ledCommError {
		t.Fatal("LED should double-blink when the inverter isn't answering")
	}
	s.enterAP(false, "test")
	if s.baseLEDMode() != ledAP {
		t.Fatal("LED should blink slowly in setup mode")
	}
}

func TestLEDActiveLow(t *testing.T) {
	w := &fakeWiFi{connected: true}
	s, now := newTestService(t, w, true)
	s.cfg.LedActiveLow = true
	g := s.cfg.GPIO.(*fakeGPIO)
	s.cfg.CommOK = func() bool { return true }

	s.wifiTick(context.Background())
	s.updateLED(*now)
	if g.led != client.GPIO_LEVEL_LOW {
		t.Fatal("an active-low LED should be driven LOW to stay steady on")
	}
	if s.ledLevelFor(false) != client.GPIO_LEVEL_HIGH {
		t.Fatal("an active-low LED should be driven HIGH to turn off")
	}
}

func TestUnreadableButtonDoesNotPanic(t *testing.T) {
	w := &fakeWiFi{connected: true}
	s, _ := newTestService(t, w, true)
	s.cfg.GPIO.(*fakeGPIO).readErr = errors.New("line busy")
	for i := 0; i < gpioErrorWarnAt+5; i++ {
		s.buttonTick()
	}
	if s.gpioErrors != gpioErrorWarnAt+5 {
		t.Fatalf("gpioErrors = %d", s.gpioErrors)
	}
}

func TestNilServiceStatus(t *testing.T) {
	var s *Service
	if st := s.Status(); st.Available {
		t.Fatal("nil service must report unavailable")
	}
}

func TestScanDedupesAndSorts(t *testing.T) {
	w := &fakeWiFi{scanResults: []*wifisvc.ScannedNetwork{
		{Ssid: "Casa", SignalStrength: -70, Security: "wpa2"},
		{Ssid: "Casa", SignalStrength: -50, Security: "wpa2"},
		{Ssid: "Vizinho", SignalStrength: -60, Security: "wpa2"},
		{Ssid: "", SignalStrength: -40},
	}}
	s, _ := newTestService(t, w, true)
	nets := s.Networks(false)
	if len(nets) != 2 || nets[0].SSID != "Casa" || nets[0].Signal != -50 || nets[1].SSID != "Vizinho" {
		t.Fatalf("networks = %+v", nets)
	}
}

func TestScanLeavesOutOwnAP(t *testing.T) {
	w := &fakeWiFi{scanResults: []*wifisvc.ScannedNetwork{
		{Ssid: "SolaStat_0002", SignalStrength: -20, Security: "wpa2"},
		{Ssid: "SolaStat_0003", SignalStrength: -60, Security: "wpa2"},
	}}
	s, _ := newTestService(t, w, true)
	nets := s.Networks(true)
	if len(nets) != 1 || nets[0].SSID != "SolaStat_0003" {
		t.Fatalf("networks = %+v", nets)
	}
}

func TestWebActivityKeepsAPOpenWhenClientCountIsZero(t *testing.T) {
	// The runtime reports 0 clients when `iw` is missing; requests to the
	// web UI must still keep the AP up.
	w := &fakeWiFi{canConnect: true, apClients: 0}
	s, now := newTestService(t, w, true)
	ctx := context.Background()
	s.enterAP(true, "test")

	*now = now.Add(apRetryEvery)
	s.NoteActivity()
	s.wifiTick(ctx)
	if !s.isAPActive() || w.stopAPs != 0 {
		t.Fatal("AP was dropped while the web UI was in use")
	}

	*now = now.Add(apActivityGrace)
	s.wifiTick(ctx)
	if s.isAPActive() {
		t.Fatal("once the phone goes quiet, the automatic AP should look for the home Wi-Fi again")
	}
}

func TestMiddlewareCountsRequests(t *testing.T) {
	w := &fakeWiFi{}
	s, now := newTestService(t, w, true)
	s.enterAP(false, "test")
	*now = now.Add(time.Minute)
	h := s.Middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/settings", nil))
	if !s.lastClientAt.Equal(*now) {
		t.Fatalf("lastClientAt = %v, want %v", s.lastClientAt, *now)
	}
	var nilSvc *Service
	if nilSvc.Middleware(h) == nil {
		t.Fatal("nil service must pass the handler through")
	}
}

func TestButtonLoopSleepsLongAtRest(t *testing.T) {
	w := &fakeWiFi{connected: true}
	s, now := newTestService(t, w, true)
	s.wifiTick(context.Background())

	s.buttonTick() // steady LED, button up
	if d := s.nextTick(*now); d != pollIdle {
		t.Fatalf("at rest nextTick = %v, want %v", d, pollIdle)
	}

	s.cfg.GPIO.(*fakeGPIO).pressed = true
	s.buttonTick() // first low sample: not debounced yet
	if d := s.nextTick(*now); d != pollActive {
		t.Fatalf("with the button going down nextTick = %v, want %v", d, pollActive)
	}
}

func TestButtonLoopWakesForLEDEdges(t *testing.T) {
	w := &fakeWiFi{}
	s, now := newTestService(t, w, true)
	s.wifiTick(context.Background()) // not connected: 1 flash (200 ms) every 3 s
	s.buttonTick()
	*now = now.Add(50 * time.Millisecond)
	if d := s.nextTick(*now); d != 150*time.Millisecond {
		t.Fatalf("nextTick = %v, want 150ms (end of the flash)", d)
	}
}

func TestLEDNextEdge(t *testing.T) {
	if d := ledNextEdge(ledOn, time.Second); d != 0 {
		t.Fatalf("steady LED has no edge, got %v", d)
	}
	if d := ledNextEdge(ledAP, 700*time.Millisecond); d != 300*time.Millisecond { // 1 s on, 300 ms left
		t.Fatalf("ledNextEdge(AP, 700ms) = %v", d)
	}
}

func TestButtonNotAnInputIsIgnoredAndReported(t *testing.T) {
	w := &fakeWiFi{connected: true}
	s, now := newTestService(t, w, true)
	g := s.cfg.GPIO.(*fakeGPIO)
	g.notInput = true
	g.pressed = true
	var events []string
	s.cfg.OnEvent = func(kind, _ string) { events = append(events, kind) }
	s.wifiTick(context.Background())

	s.checkButtonDirection(*now)
	for i := 0; i < 100; i++ {
		*now = now.Add(100 * time.Millisecond)
		s.buttonTick()
	}
	if s.btn.pressed {
		t.Fatal("a pin that isn't an input must not be read as a button")
	}
	if len(events) != 1 || events[0] != "button_unavailable" {
		t.Fatalf("events = %v, want one button_unavailable", events)
	}
	if s.baseLEDMode() != ledOn {
		t.Fatal("a button fault is reported in the log, not on the LED")
	}

	// Fixed (e.g. overlay removed): picked up on the next periodic check.
	g.notInput = false
	g.pressed = false
	*now = now.Add(dirRecheckEvery)
	s.buttonTick()
	if s.buttonFault() || s.baseLEDMode() != ledOn {
		t.Fatal("button should be usable again once the pin is an input")
	}
}

func TestShortGlitchIsNotAPress(t *testing.T) {
	var b buttonTracker
	t0 := time.Unix(0, 0)
	b.sample(true, t0)
	b.sample(true, t0.Add(100*time.Millisecond))
	b.sample(false, t0.Add(200*time.Millisecond))
	if b.pressed {
		t.Fatal("two low samples are not enough for a press")
	}
}

func TestShortPressLeavesSetupMode(t *testing.T) {
	w := &fakeWiFi{canConnect: true}
	s, now := newTestService(t, w, true)
	ctx := context.Background()
	s.enterAP(false, "button")

	s.handleButton(evShortPress, *now)
	s.handleRequest(ctx, <-s.reqs)
	if s.isAPActive() {
		t.Fatal("a short press in setup mode should go back to the home Wi-Fi")
	}

	// Outside setup mode a short press does nothing.
	s.handleButton(evShortPress, *now)
	select {
	case r := <-s.reqs:
		t.Fatalf("unexpected request %v", r.kind)
	default:
	}
}

func TestLEDSteadyWhileButtonHeld(t *testing.T) {
	w := &fakeWiFi{}
	s, now := newTestService(t, w, true)
	g := s.cfg.GPIO.(*fakeGPIO)
	s.wifiTick(context.Background()) // no Wi-Fi: normally 1 flash every 3 s
	g.pressed = true
	for i := 0; i < debounceSamples; i++ {
		*now = now.Add(100 * time.Millisecond)
		s.buttonTick()
	}
	if s.ledMode != ledHeld || g.led != client.GPIO_LEVEL_HIGH {
		t.Fatalf("LED mode %d level %v: should be steady on while the button is held", s.ledMode, g.led)
	}
	*now = now.Add(longPressHold)
	s.buttonTick()
	if s.ledMode != ledReleaseNow {
		t.Fatal("after 5 s the LED should say \"release now\"")
	}
}

func TestLEDShowsSetupModeAsSoonAsRequested(t *testing.T) {
	w := &fakeWiFi{connected: true}
	s, now := newTestService(t, w, true)
	s.cfg.CommOK = func() bool { return true }
	s.wifiTick(context.Background())
	if s.baseLEDMode() != ledOn {
		t.Fatal("precondition: all good")
	}

	s.handleButton(evLongPress, *now) // request queued, AP not started yet
	if s.isAPActive() || s.baseLEDMode() != ledAP {
		t.Fatal("the LED should switch to the setup-mode blink as soon as the long press is released")
	}
	s.handleRequest(context.Background(), <-s.reqs)
	if !s.isAPActive() || s.baseLEDMode() != ledAP {
		t.Fatal("AP should now be active")
	}
}

func TestLEDRevertsIfAPFailsToStart(t *testing.T) {
	w := &failingAPWiFi{fakeWiFi{connected: true}}
	s, now := newTestService(t, &w.fakeWiFi, true)
	s.cfg.WiFi = w
	s.cfg.CommOK = func() bool { return true }
	s.wifiTick(context.Background())

	s.handleButton(evLongPress, *now)
	s.handleRequest(context.Background(), <-s.reqs)
	if s.isAPActive() || s.baseLEDMode() != ledOn {
		t.Fatal("if the AP can't start, the LED goes back to the previous state")
	}
}

type failingAPWiFi struct{ fakeWiFi }

func (f *failingAPWiFi) StartAP(string, string, string, string, int32) (bool, error) {
	return false, errors.New("hotspot failed")
}

func TestAPStoppedOutsideSolaStatIsFollowed(t *testing.T) {
	w := &fakeWiFi{}
	s, now := newTestService(t, w, true)
	s.cfg.CommOK = func() bool { return true }
	s.enterAP(false, "button")

	// Orbit OS Settings joins the home Wi-Fi: the runtime stops the AP.
	w.mu.Lock()
	w.ap = false
	w.connected = true
	w.mu.Unlock()

	*now = now.Add(wifiCheckEvery)
	s.wifiTick(context.Background())
	if s.isAPActive() {
		t.Fatal("SolaStat must notice the AP was stopped outside it")
	}
	if !s.Status().Connected || s.baseLEDMode() != ledOn {
		t.Fatal("LED should show the client connection, not setup mode")
	}
}

func TestAPStartedOutsideSolaStatIsAdopted(t *testing.T) {
	w := &fakeWiFi{connected: true}
	s, now := newTestService(t, w, true)
	s.wifiTick(context.Background())

	w.mu.Lock()
	w.ap = true
	w.mu.Unlock()
	*now = now.Add(wifiCheckEvery)
	s.wifiTick(context.Background())
	if !s.isAPActive() || s.baseLEDMode() != ledAP {
		t.Fatal("an AP started elsewhere should show as setup mode")
	}
}

// fakeHAL is a board's LED and button through the HAL.
type fakeHAL struct {
	mu      sync.Mutex
	led     bool
	sets    int
	failLED error
	presses chan Press
}

func (h *fakeHAL) SetLED(on bool) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.failLED != nil {
		return h.failLED
	}
	h.led = on
	h.sets++
	return nil
}

func (h *fakeHAL) Presses(context.Context) (<-chan Press, error) { return h.presses, nil }

// With the HAL, the LED goes through it and the GPIO is never touched.
func TestLEDThroughTheHAL(t *testing.T) {
	w := &fakeWiFi{connected: true}
	s, now := newTestService(t, w, true)
	h := &fakeHAL{}
	s.cfg.HAL = h
	s.cfg.CommOK = func() bool { return true }
	g := s.cfg.GPIO.(*fakeGPIO)

	s.wifiTick(context.Background())
	s.buttonTick()
	if !h.led || h.sets != 1 {
		t.Fatalf("LED through the HAL: lit %v after %d writes", h.led, h.sets)
	}
	s.enterAP(false, "test")
	s.updateLED(*now)
	*now = now.Add(time.Second + time.Millisecond)
	s.updateLED(*now)
	if h.led {
		t.Fatal("the setup-mode blink should put the LED out after 1 s")
	}
	if g.sets != 0 {
		t.Fatalf("GPIO written %d times with the HAL holding the LED", g.sets)
	}
}

// A LED write that fails is tried again on the next tick, and works once
// the HAL does.
func TestLEDThroughTheHALRecovers(t *testing.T) {
	w := &fakeWiFi{connected: true}
	s, _ := newTestService(t, w, true)
	h := &fakeHAL{failLED: errors.New("HAL LED: the board announces no LED")}
	s.cfg.HAL = h
	s.cfg.CommOK = func() bool { return true }

	s.wifiTick(context.Background())
	s.buttonTick()
	s.buttonTick()
	if !s.ledFailed || s.ledKnown {
		t.Fatal("a failed LED write should be remembered as failed, not as done")
	}
	h.mu.Lock()
	h.failLED = nil
	h.mu.Unlock()
	s.buttonTick()
	if s.ledFailed || !h.led {
		t.Fatal("the LED should be lit once the HAL takes the write")
	}
}

// The HAL's long press toggles setup mode; its short press leaves it.
func TestHALPresses(t *testing.T) {
	w := &fakeWiFi{canConnect: true, connected: true}
	s, _ := newTestService(t, w, true)
	h := &fakeHAL{presses: make(chan Press, 2)}
	s.cfg.HAL = h
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.halPresses(ctx)

	h.presses <- LongPress
	select {
	case r := <-s.reqs:
		s.handleRequest(ctx, r)
	case <-time.After(5 * time.Second):
		t.Fatal("a long press should ask for setup mode")
	}
	if !s.isAPActive() {
		t.Fatal("a long press should open the AP")
	}
	h.presses <- ShortPress
	select {
	case r := <-s.reqs:
		s.handleRequest(ctx, r)
	case <-time.After(5 * time.Second):
		t.Fatal("a short press in setup mode should ask to leave it")
	}
	if s.isAPActive() {
		t.Fatal("a short press should close the AP")
	}
}

// ── Wired network and the screen ─────────────────────────────────────────────

func TestWiredNetworkKeepsAPClosed(t *testing.T) {
	w := &fakeWiFi{}
	s, now := newTestService(t, w, false)
	s.cfg.WiredUp = func() bool { return true }
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		s.wifiTick(ctx)
		*now = now.Add(apAfterDisconnect)
	}
	if s.isAPActive() || w.startAPs != 0 {
		t.Fatalf("AP opened on its own while on a wired network (starts=%d)", w.startAPs)
	}
}

func TestAutomaticAPClosesWhenWired(t *testing.T) {
	w := &fakeWiFi{}
	s, now := newTestService(t, w, false)
	wired := false
	s.cfg.WiredUp = func() bool { return wired }
	ctx := context.Background()
	s.enterAP(true, "test")

	wired = true
	s.wifiTick(ctx)
	if !s.isAPActive() {
		t.Fatal("closed within the activity grace")
	}
	*now = now.Add(apActivityGrace)
	s.wifiTick(ctx)
	if s.isAPActive() || w.stopAPs != 1 || w.connects != 0 {
		t.Fatalf("automatic AP should just close on a wired network (active=%v, stops=%d, connects=%d)", s.isAPActive(), w.stopAPs, w.connects)
	}
	// And it stays closed.
	*now = now.Add(apAfterFirstBoot)
	s.wifiTick(ctx)
	if s.isAPActive() {
		t.Fatal("AP reopened on a wired network")
	}
}

func TestManualAPStaysOpenWhenWired(t *testing.T) {
	w := &fakeWiFi{}
	s, now := newTestService(t, w, false)
	s.cfg.WiredUp = func() bool { return true }
	s.enterAP(false, "button")
	*now = now.Add(apActivityGrace)
	s.wifiTick(context.Background())
	if !s.isAPActive() {
		t.Fatal("an AP opened by hand should stay open on a wired network")
	}
}

func TestSetupForTheScreen(t *testing.T) {
	w := &fakeWiFi{}
	s, _ := newTestService(t, w, false)
	if got := s.Setup(); got != (Setup{}) {
		t.Fatalf("before setup mode: %+v", got)
	}
	s.setAPStarting(true)
	if got := s.Setup(); !got.Starting || got.Active || got.SSID != "SolaStat_0002" {
		t.Fatalf("starting: %+v", got)
	}
	s.enterAP(false, "button")
	s.setAPStarting(false)
	want := Setup{Active: true, SSID: "SolaStat_0002", Host: "192.168.4.1"}
	if got := s.Setup(); got != want {
		t.Fatalf("active, no device name: %+v, want %+v", got, want)
	}
	s.cfg.DeviceHost = "orbit-0002.local"
	want.Host = "orbit-0002.local"
	if got := s.Setup(); got != want {
		t.Fatalf("active: %+v, want %+v", got, want)
	}
	if st := s.Status(); st.APIP != "192.168.4.1" {
		t.Fatalf("status AP address = %q", st.APIP)
	}
	var nilService *Service
	if got := nilService.Setup(); got != (Setup{}) {
		t.Fatalf("nil service: %+v", got)
	}
}

// The setup network is open: a phone joins it with no password.
func TestAPHasNoPassword(t *testing.T) {
	w := &fakeWiFi{}
	s, _ := newTestService(t, w, false)
	s.enterAP(false, "button")
	if w.apPassword != "" || w.startAPs != 1 {
		t.Fatalf("started %d times, password %q", w.startAPs, w.apPassword)
	}
}

// With Config.OpenAP (the app's extension), the network opens through it, the
// captive portal asked for only off a wired network.
func TestOpenAPCaptivePortalOnlyOffWired(t *testing.T) {
	for _, wired := range []bool{false, true} {
		w := &fakeWiFi{}
		s, _ := newTestService(t, w, true)
		var opened []string
		var portal bool
		s.cfg.WiredUp = func() bool { return wired }
		s.cfg.OpenAP = func(ifname, ssid string, p bool) error {
			opened = append(opened, ssid)
			portal = p
			return w.markAP()
		}
		s.enterAP(false, "button")
		if len(opened) != 1 || opened[0] != "SolaStat_0002" || w.startAPs != 0 {
			t.Fatalf("wired=%v: opened %v, StartAP %d times", wired, opened, w.startAPs)
		}
		if portal == wired {
			t.Errorf("wired=%v: portal %v", wired, portal)
		}
		if !s.isAPActive() {
			t.Errorf("wired=%v: AP not active", wired)
		}
	}
}

func TestOpenAPErrorLeavesSetupModeOff(t *testing.T) {
	w := &fakeWiFi{}
	s, _ := newTestService(t, w, true)
	s.cfg.OpenAP = func(string, string, bool) error { return errors.New("no radio") }
	s.enterAP(false, "button")
	if s.isAPActive() || s.Status().LastError == "" {
		t.Fatalf("AP active %v, status %+v", s.isAPActive(), s.Status())
	}
}
