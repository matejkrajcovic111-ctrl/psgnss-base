# PSGNSS

**A GNSS base station in one Go binary.** Stream hub, NTRIP caster, raw archive,
RINEX conversion and a web interface — one process, one file, no runtime
dependencies.

![The dashboard](docs/images/dashboard.png)

It runs a permanent RTK reference station: it owns the receiver, serves
corrections to rovers over NTRIP, records the raw stream for post-processing,
and shows you what the sky looks like while it does it.

---

## Why it exists

The station this was written for ran on four machines: a Raspberry Pi running
`str2str`, a Windows box running STRSVR and SNIP, and a dashboard. Four places
to configure, four to restart, four to go wrong, and no single answer to "is it
working". PSGNSS replaced all of it with one binary on the Pi.

That history shaped what it is:

- **One process.** The receiver can only be held by one thing, so the thing
  that holds it also serves, archives and reports. No IPC, no glue scripts, no
  service that can be up while another is down.
- **Nothing is transcoded.** The receiver already emits both MSM4 and MSM7, so
  a mountpoint is a message-type filter over bytes it never rewrites. A rover
  receives exactly what the receiver produced.
- **It refuses rather than guesses.** An unknown configuration key stops
  startup. A frequency count that would silently drop a constellation from
  RINEX is rejected. A release that does not accept your configuration is not
  installed.

---

## Install

```sh
curl -fsSL https://raw.githubusercontent.com/OWNER/psgnss-base/main/install.sh | sudo bash
```

> **`OWNER` is a placeholder** until this repository is published; the script
> refuses to download rather than guess at a URL. Until then, clone it and run
> `sudo ./install.sh`.

One command, and it walks you through the rest. It checks the machine, fetches
a pinned Go toolchain if there isn't one (verifying its published checksum
before unpacking), builds the daemon, asks you the dozen questions it needs,
writes the configuration, the master key, the directories and the systemd
units, then starts the service and prints the URL.

```
── Receiver ──────────────────────────────────────────────

Supported boards:
  simplertk4-optimum     ZED-X20P     full support
  simplertk2b            ZED-F9P      no driver yet
  simplertk3b-pro        mosaic-X5    no driver yet
  simplertk3b-budget     UM980        no driver yet

Board [simplertk4-optimum]:
Serial devices:
 * 1) /dev/serial/by-id/usb-u-blox_AG_-_www.u-blox.com_u-blox_GNSS_receiver-if00
Receiver device [1]:
```

Every prompt has a default in brackets; enter takes it. Nothing is written
until the whole plan has been shown and agreed to.

```sh
PSGNSS_ROOT=/tmp/try sudo ./install.sh   # a whole install somewhere harmless
sudo ./psgnssd --setup --setup-dry-run   # just show the plan
```

It will not touch a station that is already configured.

**Requirements:** Linux with systemd, arm64 or amd64, and a supported receiver
on USB. A Raspberry Pi 4 is more than enough — the daemon uses about 0.35% of
one core.

**Not installed for you:** `convbin` from RTKLIB, which PSGNSS runs to produce
RINEX. Everything else works without it and the installer says so at the end.

---

## What you get

| | |
|---|---|
| **NTRIP caster** | v1 (ICY) and v2 (chunked), sourcetable, per-user accounts, connection limits, per-connection accounting, IP and expiry rules, PROXY-protocol listener |
| **Stream hub** | Owns the serial port, demultiplexes UBX and RTCM3, fans out to mountpoints, raw TCP listeners and archives |
| **Raw archive** | Local spool flushed continuously to an SMB share, daily rotation on UTC names, retention pruning |
| **RINEX** | On-demand conversion over any date range, with presets for IGN, NRCan and 1 s/30 s services |
| **Web UI** | Live skyplot and SNR, a week of history you can scrub through, users, mountpoints, receiver configuration, diagnostics, file download |
| **Receiver control** | UBX read/write with verification and automatic revert — a bad write can never orphan the stream |
| **Ephemeris** | RTCM 1019, 1042 and 1046 synthesised from raw navigation subframes, because this receiver family cannot emit them |
| **Extras** | Push-out to external casters, RTCM over UDP and serial, time to chrony without gpsd, position-integrity monitoring against a reference network |

---

## After it is running

```sh
systemctl status psgnss.service
journalctl -u psgnss.service -f

psgnssd --check --config /etc/psgnss/psgnss.toml   # validate and exit
psgnssd --config /etc/psgnss/psgnss.toml --user-add rover:password:5
psgnssd --dependencies                             # what this build is made of
```

The web UI is on port 8090. The caster is on 2101, and the raw stream is
repeated on 5001 and 5003.

**Back up `/etc/psgnss/secrets/master.key`,** somewhere other than the machine.
It is what makes stored NTRIP passwords readable. A backup holding both it and
the database is a plaintext password list, so keep them apart.

---

## Updating

```sh
psgnssd --update-check     # what is available, and what it would change
psgnssd --update           # install it
```

A release is a manifest and an Ed25519 signature over it; the verifying key is
compiled into the binary. Nothing updates on a timer — `--update` is something
a person runs.

The order matters, because this runs on a station rovers are connected to.
Signature, then size and hash, then "does the new binary run", then **does the
new binary accept this station's configuration** — all before anything is
replaced. Only then is the old binary set aside and the new one moved in; if
the restarted service does not answer, the old one goes back. The station runs
the new release or the old one, never neither.

---

## Documentation

| | |
|---|---|
| [docs/architecture.md](docs/architecture.md) | How the pieces fit, and the decisions behind them |
| [docs/configuration.md](docs/configuration.md) | Every setting, and what happens if you get it wrong |
| [docs/operations.md](docs/operations.md) | Running it: backups, restores, diagnosis, troubleshooting |
| [docs/hardware-notes.md](docs/hardware-notes.md) | What the hardware actually does, measured rather than assumed |
| [docs/releasing.md](docs/releasing.md) | Building and signing releases |
| [CHANGELOG.md](CHANGELOG.md) | What changed |
| [CREDITS.md](CREDITS.md) | Outside work this builds on, and the obligations that come with it |

---

## Hardware

| Board | Receiver | State |
|---|---|---|
| ArduSimple simpleRTK4 Optimum | u-blox ZED-X20P | Full support |
| ArduSimple simpleRTK2B | u-blox ZED-F9P | Profile only — no driver yet |
| ArduSimple simpleRTK3B Pro | Septentrio mosaic-X5 | Profile only — no driver yet |
| ArduSimple simpleRTK3B Budget | Unicore UM980 | Profile only — no driver yet |

A profile without a driver is declared and refused rather than half-supported:
the installer will not let you pick one, because PSGNSS could not configure or
read that receiver and the station would not work.

---

## Building

Go 1.24 or newer. No cgo, no C toolchain, nothing else.

```sh
make arm64      # static binary for a Pi -> dist/psgnssd-linux-arm64
make host       # for the machine you are on
make verify     # build twice, confirm the hashes match
make test vet
```

The build is reproducible: `-trimpath`, `-buildid=`, `-buildvcs=false` and
`CGO_ENABLED=0`, so two builds of the same commit are byte-identical. That is
what makes "this binary is that source" a checkable claim rather than a hope.

```
cmd/psgnssd        the daemon, and its command-line modes
internal/hub       owns the serial port, demuxes, fans out
internal/caster    NTRIP v1/v2, sourcetable, auth, accounting
internal/receiver  UBX configuration with verification and auto-revert
internal/archive   spool, share, rotation, retention
internal/web       API and the embedded single-page UI
internal/setup     the first-run installer
internal/updater   verify, install, health-check, roll back
tools/             release signing, key database generation, a UI harness
deploy/            systemd units, for a hand install
```

---

## Licence

**AGPL-3.0-or-later.** If you modify PSGNSS and make it available to others
over a network — a hosted caster or web UI counts — you must publish your
changes. See [LICENSE](LICENSE).

Several features exist because [RTKBase](https://github.com/Stefal/rtkbase)
showed the way, and [RTKLIB](https://github.com/rtklibexplorer/RTKLIB) does the
RINEX conversion and the integrity solving. No RTKBase source is copied here,
but the debt is real and the licence obligations are not optional.
[CREDITS.md](CREDITS.md) records what came from where, file by file.
