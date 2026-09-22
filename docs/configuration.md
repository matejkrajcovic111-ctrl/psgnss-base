# Configuration

Everything lives in `/etc/psgnss/psgnss.toml`. `psgnssd --setup` writes it, the
Settings page edits it, and `configs/psgnss.example.toml` documents every key
inline.

```sh
psgnssd --check --config /etc/psgnss/psgnss.toml
```

Validation is deliberately strict. An unknown key is fatal, so a typo like
`retention_dayz` stops startup instead of quietly using the default you thought
you'd changed. A few rules also reject settings that would work but produce
wrong data without telling you.

---

## `[station]`

| Key | Notes |
|---|---|
| `name` | Names the station in the UI, and by convention its mountpoints |
| `station_id` | Has to match what the receiver stamps into its observations. Use 0 unless you're also writing `CFG-RTCM DF003`. A mismatch gives rovers a healthy stream they can never fix on |
| `antenna` | Broadcast in RTCM 1008. Your surveyed coordinate was derived against a particular antenna, so changing this without re-surveying is a lie about the data |
| `receiver` | Broadcast in RTCM 1033. Blank omits it |

### `[station.position]`

| Key | Notes |
|---|---|
| `mode` | `fixed` or `survey_in` |
| `format` | `llh` or `ecef`. LLH is exactly representable and is the default |
| `latitude`, `longitude`, `height` | Degrees and ellipsoidal metres |
| `enu_offset` | Antenna reference point offset in metres. Leave at zero unless you have a calibration |

Get the position wrong and every point anyone measures moves by the same
amount, silently. Use a surveyed coordinate in a consistent reference frame: an
official ETRS89 realisation for European control, and WGS-84 only if your
survey really is WGS-84. They aren't interchangeable at centimetre precision.

Validation rejects `mode = "fixed"` with no position set.

---

## `[receiver]`

| Key | Notes |
|---|---|
| `device` | Use a `/dev/serial/by-id/…` path. `/dev/ttyACM0` depends on enumeration order, so adding a second USB serial device or rebooting with one attached moves the name, and eventually you configure the wrong receiver |
| `baud` | 9600 through 921600 |
| `profile` | Board profile ID; see the table in the README |
| `model` | Expected receiver model |
| `verify_model` | Refuse to start if the receiver reports something else |
| `revert_timeout` | Seconds before an unconfirmed configuration write rolls back |

---

## `[hub]`

Owns the serial port. `input` is `serial` in production. `tcp` and `file` exist
so the hub can run alongside another tool for comparison, or replay a capture.

`[[hub.listener]]` repeats the stream on a TCP port, optionally filtered to one
protocol (`none`, `rtcm3`, `ubx`).

---

## `[caster]`

| Key | Notes |
|---|---|
| `listen` | NTRIP caster address |
| `proxy_listen` | A second listener speaking PROXY protocol v1/v2, for traffic arriving through a reverse proxy. It's built and off unless you have something in front |
| `proxy_trusted` | CIDRs allowed to send a PROXY header. Empty trusts nobody |
| `ntrip_v1`, `ntrip_v2` | ICY and chunked HTTP |
| `operator`, `country`, `format_string`, `carrier` | Sourcetable fields. Make them describe the stream you actually serve |

### `[[caster.mountpoint]]`

A mountpoint filters; it doesn't transform.

```toml
[[caster.mountpoint]]
name        = "MyStation_MSM7"
source_id   = 1
nav_system  = "GPS+GAL+BDS"
messages    = [
  { type = 1006, interval = 10 },
  { type = 1008, interval = 10 },
  { type = 1033, interval = 10 },
  { type = 1077, interval = 1 },
  { type = 1097, interval = 1 },
  { type = 1127, interval = 1 },
]
```

`interval` is decimation in seconds, so 1 means every epoch.

Adding 1019, 1042 or 1046 turns on synthesised ephemeris for that mountpoint at
roughly 0.5 kbit/s. A rover that already has an ephemeris ignores them; one
that doesn't gets going faster.

`nav_system` should list what the stream carries and nothing more.

---

## `[archive]`

| Key | Notes |
|---|---|
| `spool_dir` | Local disk. Everything is written here first |
| `mount_point` | Where the share is mounted. Empty means local only |
| `sync_every_sec` | How often new bytes are appended to the share. Only the new bytes get copied |
| `retention_days` | 0 disables pruning |

`[archive.rtcm]`, `[archive.ubx]` and `[archive.nav]` are three independent
writers, each with its own pattern, subdirectory, rotation and filter.

- `rtcm` is the replayable correction stream.
- `nav` records `RXM-SFRBX` and `RXM-RAWX`, which is what RINEX navigation
  needs. Much smaller than a full UBX log and sufficient.
- `ubx` is the whole raw stream, off by default.

Patterns use `%Y%m%d` and `%h`, generated in UTC.

---

## `[rinex]`

| Key | Notes |
|---|---|
| `convbin_path` | RTKLIB's converter |
| `frequencies` | convbin's `-f`. Use 4 on a ZED-X20P; anything less quietly loses constellations, and startup refuses it |
| `version`, `raw_format`, `output_dir`, `compression` | |

---

## `[telemetry]`

One compact blob per epoch. `fine_rate_hz` applies for the last
`fine_window_min`, then `coarse_interval` beyond that. `retention_days` bounds
the database.

---

## `[web]`

| Key | Notes |
|---|---|
| `listen` | The UI and API |
| `display_hidden_constellations` | Which systems start hidden on the dashboard |
| `diagnostics_auto`, `diagnostics_interval_minutes` | Run the self-check on a schedule |
| `map_tiles` | An XYZ template over https with `{z}`, `{x}` and `{y}`. The browser never fetches it: psgnssd proxies and caches tiles so it can identify the station in a User-Agent, which a browser can't do and OpenStreetMap's usage policy requires. Empty means no map |
| `map_attribution` | Required if you set a tile source. Providers ask for credit and OSM's policy insists |
| `map_contact` | Goes into the outgoing User-Agent so a provider can ask you to stop |
| `map_cache_dir`, `map_cache_days` | Caching is part of the policy here, not an optimisation |

If you point `map_tiles` at OpenStreetMap's own servers and then publish your
UI, you're putting a public audience on donated infrastructure. Use a provider
you pay for, or one you host.

---

## `[update]`

| Key | Notes |
|---|---|
| `manifest_url` | https URL of the signed release envelope. Empty means the station is updated by hand, which is the default |
| `service_unit` | Restarted after a successful install |
| `binary_path` | What gets replaced. Empty means the running executable |

---

## `[security]` and `[logging]`

`key_file` is the AES-256 master key used for reversible NTRIP password
storage, root-only and 0600. It's equivalent to every stored password at once,
so treat it that way.

`logging.level` is `debug`, `info`, `warn` or `error`, and `dir` is where the
rotating log file lives.

---

## `[timesync]` and `[[rtcm_out]]`

`timesync.chrony_socket` offers receiver time to chrony's SOCK refclock. Empty
means off, which is the default, since it writes to a socket another service
owns and shouldn't start doing that after an upgrade alone. This is
USB-timestamped time, good to a few milliseconds rather than PPS.
`deploy/chrony-psgnss.conf` explains the `precision` and `delay` values, which
matter more than they look like they do.

`[[rtcm_out]]` sends a mountpoint's exact bytes to a UDP target or a serial
radio. `kind` is `udp` with a `target`, or `serial` with a `device` and `baud`.
