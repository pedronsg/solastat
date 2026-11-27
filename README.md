# SolaStat

Real-time solar monitoring for [Orbit OS](https://www.orbit-os.org) devices — **[www.solastat.com](https://www.solastat.com)**

SolaStat reads your inverter over Modbus and shows a live dashboard of solar production, battery, grid and house consumption, with history, MQTT and Home Assistant.

---

## Features

- **Live dashboard** — energy flow between PV, battery, grid and house, pushed to the browser as it changes
- **History** — per-minute readings kept on the device, with daily, monthly and yearly charts and CSV export
- **Many inverters** — built-in profiles for Deye, Solis, Huawei, GoodWe, Sofar, Swatten and more, plus your own JSON profiles
- **Faults and alarms** — the inverter's fault registers named on the dashboard and in the activity log
- **MQTT / Home Assistant** — every reading published, with Home Assistant auto-discovery
- **Setup wizard** — time zone, network, device name, inverter profile, connection test and MQTT in a few steps
- **Six languages** — English, Português, Español, Deutsch, Italiano, Français
- **Backup and restore** of the settings

---

## Connecting the inverter

| Connection | How |
|---|---|
| Modbus RTU (default) | The device's RS485 port, wired to the inverter |
| Solarman datalogger | The inverter's Solarman WiFi/LAN stick (port 8899) |
| Modbus TCP | The inverter's own Ethernet port (port 502) |
| RTU over TCP | A serial-to-Ethernet gateway (Elfin, USR, Waveshare, …) |

## Supported inverters

| Brand | Models |
|---|---|
| Swatten | SiH series |
| Deye | SUN-SG04LP3-EU three-phase, single-phase hybrid, string, microinverter |
| Solis | Hybrid (RHI-48ES-5G, S5-EH1P-L), RHI-3P HVES-5G, string 1P-5G / 3P-4G / S6-GR1P |
| Huawei | SUN2000 |
| GoodWe | ET / EH / BT / BH, ES Uniq |
| Sofar | HYD hybrid, G3 KTLX / HYD-EP, HYD-ES / ME 3000-SP, string |
| Afore | Hybrid three-phase, BNT-TL (T4, T6) |
| Others | ANENJI, Astronergy, CHINT, Hinen, INVT, KSTAR, MAXGE, Megarevo, RENAC, Renon Power, SRNE, TSUN |

Apart from Swatten, the profiles are **beta**: built from the vendors' published Modbus documents and open-source projects, not all tested on real hardware yet. Reports and fixes are welcome.

A profile is a JSON file with the connection settings and the register map. The built-in ones are in [`cmd/solastat/orb/data/inverters/`](cmd/solastat/orb/data/inverters/); a custom one can be added from the app's settings.

---

## Building

### Requirements

- Go 1.25.4+
- The [Orbit Studio](https://www.orbit-os.org/downloads.html) VS Code extension
- An Orbit OS device

Clone with the submodules (the Orbit OS SDK and the plugin API):

```bash
git clone --recursive https://github.com/pedronsg/solastat.git
```

### Build and deploy with VS Code

The source code builds with the Orbit OS extension for VS Code, **Orbit Studio**, into a ready-to-install Orbit OS app (`.orb` package):

1. Install [Orbit Studio](https://www.orbit-os.org/downloads.html) from the Orbit OS downloads page.
2. Open the cloned `solastat` folder in VS Code. The extension reads the project's settings from `orbit.project.json`.
3. In the **Orbit** panel on the sidebar, click **Build ORB**. The package is written to `.orbit/`.
4. Click **Deploy to device** to install it on your Orbit OS device.

### Building from a terminal

To check the code without packaging it:

```bash
go build ./cmd/solastat
go test ./...
```

Once installed, SolaStat opens from the Orbit OS launcher at `/solastat`. The first time, the setup wizard runs.

### Command-line flags

Useful for local development:

| Flag | Default | Description |
|---|---|---|
| `--host` | `192.168.5.226` | The Orbit OS device's address |
| `--uart` | auto | UART port |
| `--profile` | `swatten` | Inverter profile ID |
| `--data-dir` | | Where settings, history and profiles are kept (`cmd/solastat/devdata` in a checkout) |
| `--http-port` | `50000` | Web server port, within 50000–60000 |
| `--dev` | `false` | Profile Manager in the settings |

---

## Project layout

```
cmd/solastat/            the app: HTTP server and API
  web/                   dashboard, settings and wizard pages
  orb/data/inverters/    built-in inverter profiles
internal/
  solar/                 poll cycle and register decoding
  modbus/                Modbus RTU, TCP and Solarman V5
  profile/               profile format and loader
  history/               SQLite history and CSV export
  mqttclient/            MQTT and Home Assistant discovery
  eventlog/              activity log
  setupmode/             setup button, LED and setup network
  webgz/                 gzip and cached pages
  extapi/                what the app and its extension say to each other
ext/                     the app's extension point (the default adds nothing)
plugins/template/        plugin API
orbit-os-sdk-go/         Orbit OS SDK
```

---

## License

[GPL-3.0](LICENSE)
