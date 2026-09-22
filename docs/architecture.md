# Architecture

Everything runs in one process. That isn't a design preference so much as a
consequence: only one process can hold the serial port, so whatever holds it
has to do the rest too.

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

Everything below the hub is a subscriber. There's no IPC and no glue scripting,
and you can't end up with half the station running.

## Things worth knowing

### Nothing is transcoded

A mountpoint is a message-type filter over the receiver's own bytes. Since this
receiver family emits MSM4 and MSM7 together, both resolutions come out of one
stream without re-encoding anything, and a rover gets exactly what the receiver
produced.

There's one exception, and it's a single bit. The receiver marks the end of an
epoch on the last message it sends. If a mountpoint filters that message away,
the rover can't close the epoch until the next one starts, which adds a full
second to the correction age. So each mountpoint sets the marker on its own
last observation message, working on a copy and repairing the CRC. That's the
only place PSGNSS modifies anything the receiver sent.

### The receiver holds the base coordinate

The surveyed position is written into the receiver's `CFG-TMODE`, so its native
output already carries it. PSGNSS generates 1006, 1008 and 1033 from the same
configuration, because the receiver emits 1005 and never those three.

The reference station ID has to match on both sides. If the generated messages
say one thing and the observations say another, a rover will receive a
perfectly healthy stream, track satellites happily, and never fix: it can't
connect the observations to a base position. Changing the ID means writing
`CFG-RTCM` on the receiver and changing the config, together.

### Configuration is validated strictly

An unknown key stops startup rather than being ignored, so a typo like
`retention_dayz` is a failure rather than a silent default. A frequency count
that would quietly drop a constellation from RINEX is rejected outright. This
is also what lets the updater ask a new release "would you accept this
station's config?" before it replaces anything.

### Receiver writes revert themselves

Configuration goes into RAM first, with a deadline. If nothing confirms it
within the revert timeout it rolls back on its own, so a bad write can't leave
the stream orphaned. Writes touching the GNSS subsystem are chunked and paced,
because those reset the receiver and sending them in a burst drops the hub.

### Telemetry is one blob per epoch

Rather than a row per satellite-signal. A week of history, at 1 Hz for the last
hour and 30 s beyond that, stays small enough to scrub through in a browser.

### Skyplot data comes from UBX, not RTCM

Elevation, azimuth and C/N0 come from `UBX-NAV-SAT`. The outbound RTCM carries
observations, not the satellite geometry you actually want to look at.

### The archive spools locally first

Data goes to local disk and gets appended to the share continuously, not at the
daily rotation. An outage of the share costs disk space rather than data, and
can't stall the process holding the receiver. Filenames are generated in UTC;
using local time breaks continuity twice a year.

### NTRIP passwords are encrypted, not hashed

You have to be able to read a rover's password back to configure field
equipment, so they're stored as ciphertext under a master key rather than
hashed. The trade-off is real: that key is equivalent to every stored password
at once, so keep it separate from the database. Administrator logins are hashed
with argon2id and can't be recovered.

## Packages

| Package | What it owns |
|---|---|
| `internal/hub` | The serial port, protocol framing, fan-out |
| `internal/caster` | NTRIP v1/v2, sourcetable, auth, limits, accounting, PROXY protocol |
| `internal/receiver` | UBX VALGET/VALSET, verification, auto-revert, the key catalogue |
| `internal/rtcm` | Generating 1005/1006/1008/1033 from configuration |
| `internal/ephemeris` | Decoding raw navigation subframes into RTCM 1019/1042/1046 |
| `internal/archive` | Spool, share sync, rotation, retention |
| `internal/downloader` | RINEX conversion over arbitrary ranges, with service presets |
| `internal/telemetry` | Epoch blobs, decimation, retention |
| `internal/store` | SQLite schema, migrations, users, admins, sessions, accounting |
| `internal/web` | HTTP API, access rules, the embedded UI |
| `internal/setup` | The first-run interview and the files it writes |
| `internal/release`, `internal/updater` | Signed releases, verification, install with rollback |
| `internal/config` | The TOML schema and its validation |

## The web UI

A single page, served from `go:embed`. React and everything else the browser
needs are vendored rather than pulled from a CDN, because a base station might
not have internet, and a page that waits on a CDN is a page that doesn't load.

There's no build step and no Node. `app.js` is edited directly and checked with
`node --check`. Charts are uPlot; the skyplot, SNR bars and visibility heatmap
are drawn by hand in SVG and canvas. Chart colours are read from CSS custom
properties at draw time so they follow the theme switch.

`tools/uicheck` renders the interface from the working tree against a live
station in a headless browser, which is how you look at a change before
shipping it.
