package mqttclient

import (
	"encoding/json"
	"math"
	"net"
	"strings"
	"testing"
	"time"
)

func TestPayloadJSON(t *testing.T) {
	p := MakePayload(1200, 800, -400, 300, 76, math.NaN())
	p.CommOK = true
	p.AddReading("battery_voltage", "Battery Voltage", "V", 52.3)
	p.AddReading("broken", "Broken", "V", math.NaN()) // not a number: skipped
	AddLifetimeTotals(&p, 1000, 900, 300, 250, 400, 380)

	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]interface{}
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if got["comm_ok"] != true || got["inverter_fault"] != false {
		t.Fatalf("status fields: %s", b)
	}
	readings, _ := got["readings"].(map[string]interface{})
	if readings["battery_voltage"] != 52.3 || readings["broken"] != nil {
		t.Fatalf("readings: %s", b)
	}
	if got["solar_kwh_total"] != 1000.0 {
		t.Fatalf("lifetime totals: %s", b)
	}
	if _, ok := got["battery_health"]; ok {
		t.Fatalf("NaN battery health should be omitted: %s", b)
	}
	if strings.Contains(string(b), "readingMeta") || strings.Contains(string(b), "Battery Voltage") {
		t.Fatalf("reading metadata must not be sent in the state payload: %s", b)
	}
}

func TestReadingSensorMapping(t *testing.T) {
	cases := []struct {
		unit, dc, sc, wantUnit string
	}{
		{"W", "power", "measurement", "W"},
		{"V", "voltage", "measurement", "V"},
		{"A", "current", "measurement", "A"},
		{"Hz", "frequency", "measurement", "Hz"},
		{"C", "temperature", "measurement", "°C"},
		{"kWh", "energy", "total_increasing", "kWh"},
		{"%", "", "measurement", "%"},
		{"", "", "", ""},
	}
	for _, c := range cases {
		s := readingSensor("x", ReadingMeta{Label: "X", Unit: c.unit})
		if s.dc != c.dc || s.sc != c.sc || s.unit != c.wantUnit {
			t.Errorf("unit %q → dc %q sc %q unit %q, want %q %q %q", c.unit, s.dc, s.sc, s.unit, c.dc, c.sc, c.wantUnit)
		}
		if !s.live || s.category != "diagnostic" || s.id != "reading_x" {
			t.Errorf("unit %q: sensor %+v", c.unit, s)
		}
	}
	if s := readingSensor("run_state", ReadingMeta{}); s.name != "run_state" {
		t.Errorf("a reading without label should use its name, got %q", s.name)
	}
}

// What HA's Energy dashboard needs, from discovery alone: each flow's
// lifetime total in kWh, an energy total_increasing sensor, by its own name
// (HA puts the device's before it).
func TestEnergyDashboardSensors(t *testing.T) {
	cfg := DefaultConfig()
	dev := haDevice{Identifiers: []string{cfg.ClientID}, Name: cfg.DeviceName}
	byID := map[string]haConfig{}
	for _, s := range discoverySensors() {
		c := discoveryConfig(cfg, dev, cfg.TopicPrefix+"/state", s)
		byID[s.id] = c
		if strings.HasPrefix(c.Name, cfg.DeviceName) {
			t.Errorf("%s: name %q starts with the device's", s.id, c.Name)
		}
	}
	for id, key := range map[string]string{
		"solar_energy_total":              "solar_kwh_total",
		"grid_import_energy_total":        "import_kwh_total",
		"grid_export_energy_total":        "export_kwh_total",
		"battery_charged_energy_total":    "bcharged_kwh_total",
		"battery_discharged_energy_total": "bdischarged_kwh_total",
	} {
		c, ok := byID[id]
		if !ok {
			t.Errorf("no %s sensor", id)
			continue
		}
		if c.DeviceClass != "energy" || c.StateClass != "total_increasing" || c.UnitOfMeasurement != "kWh" {
			t.Errorf("%s: %s/%s in %s, want energy/total_increasing in kWh", id, c.DeviceClass, c.StateClass, c.UnitOfMeasurement)
		}
		if c.UniqueID != cfg.ClientID+"_"+id || len(c.Device.Identifiers) == 0 {
			t.Errorf("%s: unique id %q, device %+v", id, c.UniqueID, c.Device)
		}
		// A total not in the state: unavailable and an empty state (which HA
		// ignores), never 0, which it would take for a meter reset.
		if strings.Contains(c.ValueTemplate, "float(0)") || !strings.Contains(c.ValueTemplate, "value_json."+key+" is defined else ''") {
			t.Errorf("%s: template %q", id, c.ValueTemplate)
		}
		gated := false
		for _, a := range c.Availability {
			gated = gated || strings.Contains(a.ValueTemplate, "value_json."+key+" is defined")
		}
		if !gated || c.AvailabilityMode != "all" {
			t.Errorf("%s: availability %+v (%s), want it only while %s is sent", id, c.Availability, c.AvailabilityMode, key)
		}
	}
}

// TestStartDoesNotWaitForBroker: a broker refusing the login (CONNACK 5,
// as one did on a device) must not hold up Start, which runs before
// everything else in the app; the refusal shows in the status.
func TestStartDoesNotWaitForBroker(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				conn.Read(make([]byte, 512))               // CONNECT
				conn.Write([]byte{0x20, 0x02, 0x00, 0x05}) // CONNACK: not authorized
				conn.Read(make([]byte, 1))
			}()
		}
	}()

	port := ln.Addr().(*net.TCPAddr).Port
	c := New(Config{Enabled: true, Broker: "127.0.0.1", Port: port, ClientID: "test", TopicPrefix: "t"})
	done := make(chan struct{})
	go func() { c.Start(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Start waited for the broker")
	}

	deadline := time.Now().Add(5 * time.Second)
	for c.Status().Error == "" {
		if time.Now().After(deadline) {
			t.Fatal("the refusal never reached the status")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if s := c.Status(); s.Connected || !strings.Contains(strings.ToLower(s.Error), "authori") {
		t.Fatalf("status: %+v", s)
	}

	stopped := make(chan struct{})
	go func() { c.Stop(); close(stopped) }()
	select {
	case <-stopped:
	case <-time.After(3 * time.Second):
		t.Fatal("Stop hung")
	}
}
