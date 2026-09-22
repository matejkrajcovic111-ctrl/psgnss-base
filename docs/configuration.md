# Configuration

`/etc/psgnss/psgnss.toml`. `psgnssd --setup` writes it; the Settings page edits
it; `configs/psgnss.example.toml` documents every key inline.

```sh
psgnssd --check --config /etc/psgnss/psgnss.toml
```

Validation is strict on purpose. **An unknown key is fatal** — a typo like
`retention_dayz` stops startup rather than silently using a default you think
you changed. Several rules refuse configurations that would work but produce
quietly wrong data.

---

## `[station]`

| Key | Notes |
|---|---|
| `name` | Names the station in the UI and, by convention, its mountpoints |
| `station_id` | **Must match what the receiver stamps into its observations.** 0 unless you are also writing `CFG-RTCM DF003`. A mismatch gives rovers a healthy stream they can never fix on |
| `antenna` | Broadcast in RTCM 1008. The surveyed coordinate was derived against a particular antenna; changing this without re-surveying is a lie about the data |
| `receiver` | Broadcast in RTCM 1033. Blank omits it |

### `[station.position]`

| Key | Notes |
|---|---|
| `mode` | `fixed` or `survey_in` |
| `format` | `llh` or `ecef`. LLH is exactly representable and is the default |
| `latitude`, `longitude`, `height` | Degrees and ellipsoidal metres. **An error here moves every measured point by the same amount, silently.** Use a surveyed coordinate in a consistent reference frame — an official ETRS89 realisation for European control, WGS-84 only when your survey really is WGS-84. They are not interchangeable at centimetre precision |
| `enu_offset` | Antenna reference point offset, metres. Zero unless you have a calibration |

Validation refuses `mode = "fixed"` with an unset position.

---

## `[receiver]`

| Key | Notes |
|---|---|
| `device` | **Always a `/dev/serial/by-id/…` path.** `/dev/ttyACM0` is assignment-order dependent: add a second USB serial device, or reboot with one attached, and the name moves. A station addressing its receiver that way will one day configure something else |
| `baud` | 9600 … 921600 |
| `profile` | Board profile ID; see the table in the README |
| `model` | Expected receiver model |
| `verify_model` | Refuse to start if the receiver reports a different model |
| `revert_timeout` | Seconds before an unconfirmed configuration write rolls back |

---

## `[hub]`

Owns the serial port. `input` is `serial` in production; `tcp` and `file` exist
so the hub can run beside another tool for comparison, or replay a capture.

`[[hub.listener]]` repeats the stream on a TCP port with an optional protocol
filter (`none`, `rtcm3`, `ubx`).

---

## `[caster]`

| Key | Notes |
|---|---|
| `listen` | NTRIP caster address |
| `proxy_listen` | A second listener speaking PROXY protocol v1/v2, for traffic arriving through a reverse proxy. Built, and off unless something is in front of you |
| `proxy_trusted` | CIDRs allowed to send a PROXY header. Empty means nothing is trusted |
| `ntrip_v1`, `ntrip_v2` | ICY and chunked HTTP |
| `operator`, `country`, `format_string`, `carrier` | Sourcetable fields. Make them describe the stream you actually serve |

### `[[caster.mountpoint]]`

A mountpoint is a **filter**, not a transformation.

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

`interval` is decimation in seconds; 1 is every epoch. Adding 1019, 1042 or
1046 turns on synthesised ephemeris for that mountpoint, at about 0.5 kbit/s —
a rover that already holds an ephemeris ignores them, one that does not starts
faster.

`nav_system` should name what the stream carries and nothing else.

---

## `[archive]`

| Key | Notes |
|---|---|
| `spool_dir` | Local disk. Everything is written here first |
| `mount_point` | Where the share is mounted. Empty means local-only |
| `sync_every_sec` | How often new bytes are appended to the share. Only the new bytes are copied |
| `retention_days` | 0 disables pruning |

`[archive.rtcm]`, `[archive.ubx]` and `[archive.nav]` are three independent
writers with their own pattern, subdirectory, rotation and filter.

- **`rtcm`** is the replayable correction stream.
- **`nav`** records `RXM-SFRBX` and `RXM-RAWX` — the RINEX navigation source.
  Smaller than a full UBX log, and sufficient.
- **`ubx`** is the whole raw stream, off by default.

Patterns use `%Y%m%d` and `%h`, **generated in UTC**.

---

## `[rinex]`

| Key | Notes |
|---|---|
| `convbin_path` | RTKLIB's converter |
| `frequencies` | convbin's `-f`. **4 on a ZED-X20P**; less silently loses constellations and startup refuses it |
| `version`, `raw_format`, `output_dir`, `compression` | |

---

## `[telemetry]`

One compact blob per epoch. `fine_rate_hz` for the last `fine_window_min`, then
`coarse_interval` beyond it. `retention_days` bounds the database.

---

## `[web]`

| Key | Notes |
|---|---|
| `listen` | The UI and API |
| `display_hidden_constellations` | Which systems start hidden on the dashboard |
| `diagnostics_auto`, `diagnostics_interval_minutes` | Run the self-check on a schedule |
| `map_tiles` | An XYZ template, https, with `{z}`/`{x}`/`{y}`. **The browser never fetches it** — psgnssd proxies and caches tiles so it can identify this station in a User-Agent, which a browser cannot do and OpenStreetMap's usage policy requires. Empty means no map |
| `map_attribution` | Required with a tile source. Providers ask for credit; OSM's policy insists |
| `map_contact` | Goes into the outgoing User-Agent so a provider can ask you to stop |
| `map_cache_dir`, `map_cache_days` | Caching is part of the policy, not an optimisation |

> If you point `map_tiles` at OpenStreetMap's own servers and then publish your
> UI, you are putting a public audience on donated infrastructure. Use a
> provider you pay or one you host.

---

## `[update]`

| Key | Notes |
|---|---|
| `manifest_url` | https URL of the signed release envelope. Empty means this station is updated by hand, which is the default |
| `service_unit` | Restarted after a successful install |
| `binary_path` | What gets replaced. Empty means the running executable |

---

## `[security]` and `[logging]`

`key_file` is the AES-256 master key for reversible NTRIP password storage,
root-only and 0600. **It is equivalent to every stored password at once.**

`logging.level` is `debug`, `info`, `warn` or `error`; `dir` is where the
rotating log lives.

---

## `[timesync]` and `[[rtcm_out]]`

`timesync.chrony_socket` offers receiver time to chrony's SOCK refclock. Empty
means off, which is the default: it writes to a socket another service owns and
should never start doing so after an upgrade alone. This is USB-timestamped
time — good to a few milliseconds, not PPS. `deploy/chrony-psgnss.conf`
explains the `precision` and `delay` values, which are not decoration.

`[[rtcm_out]]` sends a mountpoint's exact bytes to a UDP target or a serial
radio. `kind` is `udp` (with `target`) or `serial` (with `device` and `baud`).
