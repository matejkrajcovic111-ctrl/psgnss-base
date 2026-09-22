# Architecture

One process, because only one process can hold the receiver.

```
receiver ──USB──▶ psgnssd
                    │
                    ├─ hub          owns /dev/serial/by-id/…, demuxes UBX and RTCM3
                    │   ├─ caster        :2101   NTRIP v1/v2, one mountpoint per filter
                    │   ├─ raw TCP       :5003, :5001   byte-for-byte passthrough
                    │   ├─ archive       spool ──▶ SMB share
                    │   ├─ rtcm out      UDP and serial
                    │   ├─ push-out      to external casters
                    │   ├─ ephemeris     UBX subframes ──▶ RTCM 1019/1042/1046
                    │   ├─ timesync      NAV-PVT ──▶ chrony SOCK refclock
                    │   └─ telemetry     one blob per epoch ──▶ SQLite
                    │
                    └─ web          :8090  API and the embedded UI
```

Everything downstream of the hub is a subscriber. Nothing else opens the serial
port, and nothing can be running while another part is not.

## The decisions worth knowing

### Nothing is transcoded

A mountpoint is a **message-type filter** over the receiver's own bytes. This
receiver family emits MSM4 and MSM7 concurrently, so both resolutions can be
served from one stream without re-encoding anything. A rover receives exactly
the bytes the receiver produced.

The one exception is a single bit: the receiver marks the end of an epoch on
the last message it emits. A mountpoint serving a subset would filter that
marker away, leaving a rover unable to close the epoch until the next one
started — a full second of extra correction age. Each mountpoint therefore sets
the marker on its own last observation message, on a copy, with the CRC
repaired. That is the only place PSGNSS alters a receiver message.

### The receiver holds the base coordinate

The surveyed position is written into the receiver's `CFG-TMODE`, so its native
output already carries it. PSGNSS generates 1006, 1008 and 1033 from the same
configuration because the receiver emits 1005 and never those three.

**The reference station ID must match on both sides.** If the generated station
messages carry one ID and the receiver's observations carry another, a rover
receives a perfectly healthy stream, tracks satellites and never fixes: it
cannot associate the observations with a base position. Changing the ID means
writing `CFG-RTCM` on the receiver *and* changing the configuration, together.

### Configuration is validated, strictly

An unknown key stops startup rather than being ignored — a typo like
`retention_dayz` is a failure, not a silent default. A frequency count that
would drop a constellation from RINEX output is refused. Validation is the
reason the updater can ask a new release "would you accept this station's
configuration?" before replacing anything.

### Receiver writes revert themselves

Configuration goes to RAM first with a deadline. If it is not confirmed within
the revert timeout it is rolled back automatically, so a bad write can never
orphan the stream. Writes that touch the GNSS subsystem are chunked and paced,
because those reset the receiver and a burst would drop the hub.

### Telemetry is one blob per epoch

Not a row per satellite-signal. A week of history at 1 Hz for the last hour and
30 s beyond it stays small enough to scrub through in a browser.

### Skyplot data comes from UBX, not RTCM

Elevation, azimuth and C/N0 come from `UBX-NAV-SAT`. The outbound RTCM streams
carry observations, not the satellite geometry a person wants to look at.

### The archive spools locally first

Data is written to local disk and appended to the share continuously, not at
the daily rotation. An outage of the share costs disk space rather than data,
and can never stall the process that owns the receiver. Filenames are generated
in **UTC**; formatting them in local time breaks continuity twice a year.

### NTRIP passwords are encrypted, not hashed

An operator has to be able to read a rover's password back to configure field
equipment, so they are stored as ciphertext under a master key rather than as
hashes. That is a deliberate trade-off and it has a consequence: **the master
key is equivalent to every stored password at once.** Back it up separately
from the database. Administrator logins are hashed with argon2id and are not
recoverable.

## Packages

| Package | What it owns |
|---|---|
| `internal/hub` | The serial port, protocol framing, fan-out |
| `internal/caster` | NTRIP v1/v2, sourcetable, auth, limits, accounting, PROXY protocol |
| `internal/receiver` | UBX VALGET/VALSET, verification, auto-revert, the key catalogue |
| `internal/rtcm` | Generation of 1005/1006/1008/1033 from configuration |
| `internal/ephemeris` | Decoding raw navigation subframes into RTCM 1019/1042/1046 |
| `internal/archive` | Spool, share sync, rotation, retention |
| `internal/downloader` | RINEX conversion over arbitrary ranges, with service presets |
| `internal/telemetry` | Epoch blobs, decimation, retention |
| `internal/store` | SQLite schema, migrations, users, admins, sessions, accounting |
| `internal/web` | HTTP API, access rules, the embedded UI |
| `internal/setup` | The first-run interview, the plan, the files it writes |
| `internal/release`, `internal/updater` | Signed releases, verification, install with rollback |
| `internal/config` | The TOML schema and its validation |

## The web UI

A single page, served from `go:embed`. React and every other browser dependency
are **vendored**, not fetched from a CDN: a base station may have no internet,
and a page that waits on a CDN is a page that does not load.

There is no build step and no Node. `app.js` is edited directly and checked
with `node --check`; the charts are uPlot, and the skyplot, SNR bars and
visibility heatmap are drawn by hand in SVG and canvas. Chart colours are read
from CSS custom properties at draw time, so they follow the theme switch.

`tools/uicheck` renders the interface from the working tree against a live
station, in a headless browser, so a change can be looked at before it ships.
