package mqttclient

import (
	"encoding/json"
	"fmt"
	"math"
	"sync"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"

	"github.com/OrbitOS-org/orbit-os-sdk-go/v26/logger"
)

const logTag = "mqtt"

// connectRetry is how long the first connection waits between attempts.
const connectRetry = 10 * time.Second

// Config holds MQTT broker connection settings, persisted to disk.
type Config struct {
	Enabled         bool   `json:"enabled"`
	Broker          string `json:"broker"`
	Port            int    `json:"port"`
	Username        string `json:"username"`
	Password        string `json:"password"`
	ClientID        string `json:"client_id"`
	TopicPrefix     string `json:"topic_prefix"`
	DiscoveryPrefix string `json:"discovery_prefix"`
	DeviceName      string `json:"device_name"`
}

// DefaultConfig returns safe defaults.
func DefaultConfig() Config {
	return Config{
		Enabled:         false,
		Port:            1883,
		ClientID:        "solastat",
		TopicPrefix:     "solastat",
		DiscoveryPrefix: "homeassistant",
		DeviceName:      "SolaStat",
	}
}

// Status is returned to the frontend.
type Status struct {
	Connected bool   `json:"connected"`
	Error     string `json:"error,omitempty"`
	LastSent  string `json:"last_sent,omitempty"`
}

// Payload is published to <topic_prefix>/state on every inverter poll.
type Payload struct {
	SolarW        *float64 `json:"solar_w"`
	LoadW         *float64 `json:"load_w"`
	GridW         *float64 `json:"grid_w"`
	BatteryW      *float64 `json:"battery_w"`
	BatterySOC    *float64 `json:"battery_soc"`
	BatteryHealth *float64 `json:"battery_health,omitempty"`

	// Cumulative energy totals for today (kWh) — for HA Energy Dashboard.
	SolarKWh       *float64 `json:"solar_kwh,omitempty"`
	LoadKWh        *float64 `json:"load_kwh,omitempty"`
	ImportKWh      *float64 `json:"import_kwh,omitempty"`
	ExportKWh      *float64 `json:"export_kwh,omitempty"`
	BChargedKWh    *float64 `json:"bcharged_kwh,omitempty"`
	BDischargedKWh *float64 `json:"bdischarged_kwh,omitempty"`

	// Lifetime energy totals (kWh) since the history started. They never
	// reset at midnight, so they are the most robust source for the HA
	// Energy Dashboard (no dependence on the daily reset or restarts).
	SolarKWhTotal       *float64 `json:"solar_kwh_total,omitempty"`
	LoadKWhTotal        *float64 `json:"load_kwh_total,omitempty"`
	ImportKWhTotal      *float64 `json:"import_kwh_total,omitempty"`
	ExportKWhTotal      *float64 `json:"export_kwh_total,omitempty"`
	BChargedKWhTotal    *float64 `json:"bcharged_kwh_total,omitempty"`
	BDischargedKWhTotal *float64 `json:"bdischarged_kwh_total,omitempty"`

	// Status: whether the inverter answered the last poll, and whether it
	// reports a fault (non-zero status register, when the profile has one).
	CommOK        bool `json:"comm_ok"`
	InverterFault bool `json:"inverter_fault"`

	// Every reading of the active profile, keyed by register name, so
	// values like battery voltage/temperature or grid frequency reach HA
	// too. readingMeta (label, unit) drives their discovery and is not sent.
	Readings    map[string]float64     `json:"readings,omitempty"`
	readingMeta map[string]ReadingMeta `json:"-"`
}

// ReadingMeta describes a profile reading for HA discovery.
type ReadingMeta struct {
	Label string
	Unit  string
}

func floatPtr(v float64) *float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return nil
	}
	return &v
}

// MakePayload builds a Payload from raw register values (NaN = missing).
func MakePayload(solarW, loadW, gridW, battW, soc, health float64) Payload {
	return Payload{
		SolarW:        floatPtr(solarW),
		LoadW:         floatPtr(loadW),
		GridW:         floatPtr(gridW),
		BatteryW:      floatPtr(battW),
		BatterySOC:    floatPtr(soc),
		BatteryHealth: floatPtr(health),
	}
}

// AddReading adds one profile reading (skipped if not a finite number).
func (p *Payload) AddReading(name, label, unit string, v float64) {
	if name == "" || math.IsNaN(v) || math.IsInf(v, 0) {
		return
	}
	if p.Readings == nil {
		p.Readings = map[string]float64{}
		p.readingMeta = map[string]ReadingMeta{}
	}
	p.Readings[name] = v
	p.readingMeta[name] = ReadingMeta{Label: label, Unit: unit}
}

// AddLifetimeTotals attaches the never-resetting kWh totals.
func AddLifetimeTotals(p *Payload, solar, load, importKWh, exportKWh, bcharged, bdischarged float64) {
	p.SolarKWhTotal = floatPtr(solar)
	p.LoadKWhTotal = floatPtr(load)
	p.ImportKWhTotal = floatPtr(importKWh)
	p.ExportKWhTotal = floatPtr(exportKWh)
	p.BChargedKWhTotal = floatPtr(bcharged)
	p.BDischargedKWhTotal = floatPtr(bdischarged)
}

// AddTotals attaches today's cumulative kWh totals to an existing Payload.
func AddTotals(p *Payload, solar, load, importKWh, exportKWh, bcharged, bdischarged float64) {
	p.SolarKWh = floatPtr(solar)
	p.LoadKWh = floatPtr(load)
	p.ImportKWh = floatPtr(importKWh)
	p.ExportKWh = floatPtr(exportKWh)
	p.BChargedKWh = floatPtr(bcharged)
	p.BDischargedKWh = floatPtr(bdischarged)
}

// Client manages the MQTT connection and HA discovery.
type Client struct {
	mu     sync.RWMutex
	cfg    Config
	mc     mqtt.Client
	status Status
	// stopConnect ends the first connection's attempts (connectLoop).
	stopConnect chan struct{}
	lastSent    time.Time

	// Profile readings seen so far (name → label/unit), and which of them
	// already have an HA discovery config on the current connection.
	readings  map[string]ReadingMeta
	announced map[string]bool
}

func (c *Client) availabilityTopic() string { return c.cfg.TopicPrefix + "/availability" }

// New creates a Client. Call Start or Reconfigure to connect.
func New(cfg Config) *Client {
	return &Client{cfg: cfg}
}

// Start connects to the broker if enabled.
func (c *Client) Start() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cfg.Enabled && c.cfg.Broker != "" {
		c.connectLocked()
	}
}

// Stop disconnects gracefully.
func (c *Client) Stop() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.stopConnect != nil {
		close(c.stopConnect)
		c.stopConnect = nil
	}
	if c.mc != nil {
		// A clean disconnect doesn't fire the Last Will, so say it ourselves.
		if c.status.Connected {
			c.mc.Publish(c.availabilityTopic(), 1, true, "offline").WaitTimeout(time.Second)
		}
		c.mc.Disconnect(500)
		c.mc = nil
	}
	c.status.Connected = false
}

// Reconfigure stops the current connection and starts a new one.
func (c *Client) Reconfigure(cfg Config) {
	c.Stop()
	c.mu.Lock()
	c.cfg = cfg
	c.mu.Unlock()
	if cfg.Enabled && cfg.Broker != "" {
		c.mu.Lock()
		c.connectLocked()
		c.mu.Unlock()
	}
}

// Status returns a snapshot of the current connection state.
func (c *Client) Status() Status {
	c.mu.RLock()
	defer c.mu.RUnlock()
	s := c.status
	if !c.lastSent.IsZero() {
		s.LastSent = c.lastSent.Format(time.RFC3339)
	}
	return s
}

// Publish sends a state payload to <topic_prefix>/state. No-op if not connected.
// Connected reports whether the broker connection is up, so callers can skip
// building a payload nobody will receive.
func (c *Client) Connected() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.mc != nil && c.status.Connected
}

func (c *Client) Publish(p Payload) {
	c.mu.RLock()
	mc := c.mc
	connected := c.status.Connected
	prefix := c.cfg.TopicPrefix
	c.mu.RUnlock()

	if mc == nil || !connected {
		return
	}
	c.announceReadings(mc, p.readingMeta)
	b, err := json.Marshal(p)
	if err != nil {
		return
	}
	tok := mc.Publish(prefix+"/state", 0, true, b)
	tok.Wait()
	if tok.Error() == nil {
		c.mu.Lock()
		c.lastSent = time.Now()
		c.mu.Unlock()
	}
}

func (c *Client) connectLocked() {
	opts := mqtt.NewClientOptions()
	broker := fmt.Sprintf("tcp://%s:%d", c.cfg.Broker, c.cfg.Port)
	opts.AddBroker(broker)
	opts.SetClientID(c.cfg.ClientID)
	if c.cfg.Username != "" {
		opts.SetUsername(c.cfg.Username)
		opts.SetPassword(c.cfg.Password)
	}
	// Once up, paho reconnects by itself; the first connection is
	// connectLoop's.
	opts.SetAutoReconnect(true)
	// Last Will: if SolaStat dies or loses the network, the broker marks it
	// offline and HA shows every SolaStat entity as unavailable.
	opts.SetWill(c.availabilityTopic(), "offline", 1, true)

	opts.SetOnConnectHandler(func(mc mqtt.Client) {
		logger.Infof(logTag, "connected to %s", broker)
		c.mu.Lock()
		c.status.Connected = true
		c.status.Error = ""
		c.announced = map[string]bool{} // a (re)connect re-sends every discovery config
		known := make(map[string]ReadingMeta, len(c.readings))
		for k, v := range c.readings {
			known[k] = v
		}
		avail := c.availabilityTopic()
		c.mu.Unlock()
		c.publishDiscovery(mc)
		c.announceReadings(mc, known)
		mc.Publish(avail, 1, true, "online").Wait()
	})
	opts.SetConnectionLostHandler(func(_ mqtt.Client, err error) {
		logger.Warnf(logTag, "connection lost: %v", err)
		c.mu.Lock()
		c.status.Connected = false
		c.status.Error = err.Error()
		c.mu.Unlock()
	})

	mc := mqtt.NewClient(opts)
	stop := make(chan struct{})
	c.mc, c.stopConnect = mc, stop
	go c.connectLoop(mc, broker, stop)
}

// connectLoop makes mc's first connection, again every connectRetry until
// it is up or Stop is called, without holding up the caller: with paho's
// own retry the connect token only completed once connected, so a broker
// refusing the login ("not authorized") kept the app from ever starting.
// A failure is logged when it changes and shown in the status.
func (c *Client) connectLoop(mc mqtt.Client, broker string, stop <-chan struct{}) {
	var last string
	for {
		tok := mc.Connect()
		tok.Wait()
		err := tok.Error()
		if err == nil {
			return
		}
		select {
		case <-stop:
			return
		default:
		}
		if err.Error() != last {
			last = err.Error()
			logger.Errorf(logTag, "connect to %s failed: %v (retrying every %s)", broker, err, connectRetry)
		}
		c.mu.Lock()
		if c.mc == mc {
			c.status.Connected = false
			c.status.Error = err.Error()
		}
		c.mu.Unlock()
		select {
		case <-stop:
			return
		case <-time.After(connectRetry):
		}
	}
}

// ── HA MQTT Discovery ─────────────────────────────────────────────────────────

type haDevice struct {
	Identifiers  []string `json:"identifiers"`
	Name         string   `json:"name"`
	Model        string   `json:"model"`
	Manufacturer string   `json:"manufacturer"`
}

type haAvailability struct {
	Topic         string `json:"topic"`
	ValueTemplate string `json:"value_template,omitempty"`
}

type haConfig struct {
	Name              string           `json:"name"`
	UniqueID          string           `json:"unique_id"`
	StateTopic        string           `json:"state_topic"`
	ValueTemplate     string           `json:"value_template"`
	UnitOfMeasurement string           `json:"unit_of_measurement,omitempty"`
	DeviceClass       string           `json:"device_class,omitempty"`
	StateClass        string           `json:"state_class,omitempty"`
	Icon              string           `json:"icon,omitempty"`
	EntityCategory    string           `json:"entity_category,omitempty"`
	Availability      []haAvailability `json:"availability,omitempty"`
	AvailabilityMode  string           `json:"availability_mode,omitempty"`
	Device            haDevice         `json:"device"`
}

type haSensor struct {
	component string // "sensor" or "binary_sensor"
	id        string
	name      string
	tmpl      string
	unit      string
	dc        string
	sc        string
	icon      string
	category  string
	// live: only available while the inverter answers (power, SOC, raw
	// readings). Energy totals come from the history DB and stay valid.
	live bool
	// field: available only while the state has this key (an energy
	// total): a total not known is unavailable, never 0, which a
	// total_increasing sensor would take for a meter reset, and then count
	// the whole total over again when it came back.
	field string
}

func (c *Client) discoveryBase() (Config, haDevice, string) {
	c.mu.RLock()
	cfg := c.cfg
	c.mu.RUnlock()
	dev := haDevice{
		Identifiers:  []string{cfg.ClientID},
		Name:         cfg.DeviceName,
		Model:        "SolaStat Monitor",
		Manufacturer: "SolaStat",
	}
	return cfg, dev, cfg.TopicPrefix + "/state"
}

func (c *Client) publishSensor(mc mqtt.Client, cfg Config, dev haDevice, stateTopic string, s haSensor) {
	component := s.component
	if component == "" {
		component = "sensor"
	}
	b, _ := json.Marshal(discoveryConfig(cfg, dev, stateTopic, s))
	topic := fmt.Sprintf("%s/%s/%s/config", cfg.DiscoveryPrefix, component, cfg.ClientID+"_"+s.id)
	tok := mc.Publish(topic, 1, true, b)
	tok.Wait()
	if err := tok.Error(); err != nil {
		logger.Warnf(logTag, "discovery publish %s: %v", topic, err)
	} else {
		logger.Debugf(logTag, "HA discovery: %s", topic)
	}
}

// discoveryConfig is sensor s's HA discovery config. Its name is the
// entity's own ("Solar Power"): HA puts the device's name before it.
func discoveryConfig(cfg Config, dev haDevice, stateTopic string, s haSensor) haConfig {
	avail := []haAvailability{{Topic: cfg.TopicPrefix + "/availability"}}
	mode := ""
	if s.live {
		avail = append(avail, haAvailability{
			Topic:         stateTopic,
			ValueTemplate: "{{ 'online' if value_json.comm_ok else 'offline' }}",
		})
		mode = "all"
	}
	if s.field != "" {
		avail = append(avail, haAvailability{
			Topic:         stateTopic,
			ValueTemplate: "{{ 'online' if value_json." + s.field + " is defined else 'offline' }}",
		})
		mode = "all"
	}
	return haConfig{
		Name:              s.name,
		UniqueID:          cfg.ClientID + "_" + s.id,
		StateTopic:        stateTopic,
		ValueTemplate:     s.tmpl,
		UnitOfMeasurement: s.unit,
		DeviceClass:       s.dc,
		StateClass:        s.sc,
		Icon:              s.icon,
		EntityCategory:    s.category,
		Availability:      avail,
		AvailabilityMode:  mode,
		Device:            dev,
	}
}

func (c *Client) publishDiscovery(mc mqtt.Client) {
	cfg, dev, stateTopic := c.discoveryBase()
	for _, s := range discoverySensors() {
		c.publishSensor(mc, cfg, dev, stateTopic, s)
	}
}

// discoverySensors are SolaStat's own sensors: power, battery, energy (the
// lifetime totals are what HA's Energy dashboard takes), status.
func discoverySensors() []haSensor {
	energy := func(id, name, key, icon string) haSensor {
		// Empty, not 0, when the state has no such total: HA ignores an
		// empty state and keeps the last (see haSensor.field).
		return haSensor{id: id, name: name,
			tmpl: "{{ value_json." + key + " | float | round(3) if value_json." + key + " is defined else '' }}",
			unit: "kWh", dc: "energy", sc: "total_increasing", icon: icon, field: key}
	}
	sensors := []haSensor{
		{id: "solar_power", name: "Solar Power", tmpl: "{{ value_json.solar_w | float(0) | round(1) }}", unit: "W", dc: "power", sc: "measurement", icon: "mdi:solar-power", live: true},
		{id: "load_power", name: "Load Power", tmpl: "{{ value_json.load_w | float(0) | round(1) }}", unit: "W", dc: "power", sc: "measurement", icon: "mdi:home-lightning-bolt", live: true},
		{id: "grid_power", name: "Grid Power", tmpl: "{{ value_json.grid_w | float(0) | round(1) }}", unit: "W", dc: "power", sc: "measurement", icon: "mdi:transmission-tower", live: true},
		{id: "battery_power", name: "Battery Power", tmpl: "{{ value_json.battery_w | float(0) | round(1) }}", unit: "W", dc: "power", sc: "measurement", icon: "mdi:battery-charging", live: true},
		{id: "battery_soc", name: "Battery SOC", tmpl: "{{ value_json.battery_soc | float(0) | round(1) }}", unit: "%", dc: "battery", sc: "measurement", live: true},
		{id: "battery_health", name: "Battery Health", tmpl: "{{ value_json.battery_health | float(0) | round(1) }}", unit: "%", sc: "measurement", icon: "mdi:battery-heart-variant", live: true},
		// Energy of the day (reset at local midnight, in the SolaStat timezone).
		energy("solar_energy", "Solar Energy Today", "solar_kwh", "mdi:solar-power"),
		energy("load_energy", "Load Energy Today", "load_kwh", "mdi:home-lightning-bolt"),
		energy("grid_import_energy", "Grid Import Today", "import_kwh", "mdi:transmission-tower"),
		energy("grid_export_energy", "Grid Export Today", "export_kwh", "mdi:transmission-tower-export"),
		energy("battery_charged_energy", "Battery Charged Today", "bcharged_kwh", "mdi:battery-arrow-up"),
		energy("battery_discharged_energy", "Battery Discharged Today", "bdischarged_kwh", "mdi:battery-arrow-down"),
		// Lifetime energy (never resets): recommended for the Energy Dashboard.
		energy("solar_energy_total", "Solar Energy Total", "solar_kwh_total", "mdi:solar-power"),
		energy("load_energy_total", "Load Energy Total", "load_kwh_total", "mdi:home-lightning-bolt"),
		energy("grid_import_energy_total", "Grid Import Total", "import_kwh_total", "mdi:transmission-tower"),
		energy("grid_export_energy_total", "Grid Export Total", "export_kwh_total", "mdi:transmission-tower-export"),
		energy("battery_charged_energy_total", "Battery Charged Total", "bcharged_kwh_total", "mdi:battery-arrow-up"),
		energy("battery_discharged_energy_total", "Battery Discharged Total", "bdischarged_kwh_total", "mdi:battery-arrow-down"),
		// Status (problem = ON when something is wrong), usable in automations.
		{component: "binary_sensor", id: "comm_problem", name: "Inverter Communication Problem",
			tmpl: "{{ 'OFF' if value_json.comm_ok else 'ON' }}", dc: "problem", category: "diagnostic"},
		{component: "binary_sensor", id: "inverter_fault", name: "Inverter Fault",
			tmpl: "{{ 'ON' if value_json.inverter_fault else 'OFF' }}", dc: "problem", category: "diagnostic"},
	}
	return sensors
}

// announceReadings publishes a discovery config for every profile reading
// not yet announced on this connection. Readings are only known once the
// inverter has been polled, so this runs from Publish as they appear.
func (c *Client) announceReadings(mc mqtt.Client, metas map[string]ReadingMeta) {
	if len(metas) == 0 {
		return
	}
	var fresh []string
	c.mu.Lock()
	if c.readings == nil {
		c.readings = map[string]ReadingMeta{}
	}
	if c.announced == nil {
		c.announced = map[string]bool{}
	}
	for name, m := range metas {
		c.readings[name] = m
		if !c.announced[name] {
			c.announced[name] = true
			fresh = append(fresh, name)
		}
	}
	c.mu.Unlock()
	if len(fresh) == 0 {
		return
	}
	cfg, dev, stateTopic := c.discoveryBase()
	for _, name := range fresh {
		c.publishSensor(mc, cfg, dev, stateTopic, readingSensor(name, metas[name]))
	}
}

// readingSensor maps a raw profile reading to an HA sensor, guessing the
// device class from its unit.
func readingSensor(name string, m ReadingMeta) haSensor {
	label := m.Label
	if label == "" {
		label = name
	}
	s := haSensor{
		id:       "reading_" + name,
		name:     label,
		tmpl:     "{{ (value_json.readings | default({}))['" + name + "'] | default(0) }}",
		unit:     m.Unit,
		sc:       "measurement",
		category: "diagnostic",
		live:     true,
	}
	switch m.Unit {
	case "W":
		s.dc = "power"
	case "kW":
		s.dc = "power"
	case "V":
		s.dc = "voltage"
	case "A":
		s.dc = "current"
	case "Hz":
		s.dc = "frequency"
	case "°C", "C":
		s.dc, s.unit = "temperature", "°C"
	case "kWh", "Wh":
		s.dc, s.sc = "energy", "total_increasing"
	case "%":
		s.sc = "measurement"
	case "":
		s.sc = "" // status codes and similar: no statistics
	}
	return s
}
