# Changelog

## 1.0.0

First public release. The station this was built for has been running in
production since 2026-09-15, serving corrections to real rovers. This is that
software, with its development history left behind in a separate archive.

### The station

- A stream hub that owns the receiver, demultiplexes UBX and RTCM3, and fans
  the result out to everything else.
- An NTRIP caster, v1 and v2, with a sourcetable, per-user accounts, connection
  limits, per-connection accounting, expiry and IP rules, per-mountpoint
  access, and a PROXY-protocol listener for deployments behind a reverse proxy.
- Mountpoints as filters. The receiver emits MSM4 and MSM7 together, so both
  come from one stream and nothing is re-encoded.
- Receiver control over UBX with read-back verification and automatic revert.
- Two archives, the RTCM correction stream and a navigation archive of
  `RXM-SFRBX` plus `RXM-RAWX`, spooled locally and flushed continuously to an
  SMB share, rotated on UTC names and pruned on a retention policy.
- RINEX conversion over arbitrary ranges, including overnight spans and the
  current day, with presets for IGN, NRCan and 1 s / 30 s services.
- Telemetry as one compact blob per epoch, decimated, about a week deep.
- A web interface: live skyplot and per-band SNR, a scrubable week of history,
  users, mountpoints, receiver configuration, diagnostics and file download.
  Single page, vendored dependencies, no CDN and no build step.

### Beyond the original plan

- Synthesised ephemeris. This receiver family has no configuration key for RTCM
  1019, 1042 or 1046 and never will, so they're decoded from raw navigation
  subframes. Every decoder was checked field by field against RTKLIB's
  `convbin` over the same captured bytes.
- Push-out to external casters, RTCM over UDP and serial, and time to chrony
  through a SOCK refclock without gpsd.
- Position-integrity monitoring, which periodically re-solves the antenna from
  its own live observations against a reference network and compares that with
  the broadcast position.
- Settings backup and restore in one encrypted file, enough to rebuild the
  station on new hardware.
- Public read-only pages, so a dashboard can be published without exposing
  anything else.

### Installing and updating

- `install.sh` takes a bare machine to a running station in one command. It
  fetches a pinned Go toolchain and verifies its published checksum if the
  machine doesn't have one, builds, runs the first-run interview and starts the
  service.
- `psgnssd --setup` is the interview on its own. Answers become a plan, and
  only the plan touches the disk, so it can be shown, dry-run, or written to a
  temporary root first. It won't overwrite a configured station.
- Signed releases and a self-updater that verifies the signature, the size and
  the hash, asks the new binary whether it accepts this station's
  configuration, and puts the old binary back if the restarted service doesn't
  answer.
- A dependency inventory with no hand-typed version numbers, carried in each
  release so a station can see what an update would change before taking it.

### Known limits

- Only the u-blox ZED-X20P has a driver. The ZED-F9P, mosaic-X5 and UM980 have
  board profiles and are refused rather than half-supported.
- `convbin` isn't installed for you, and RINEX conversion needs it.
- The interface is English only.
- Alerting, self-registration and multi-base support aren't implemented.
