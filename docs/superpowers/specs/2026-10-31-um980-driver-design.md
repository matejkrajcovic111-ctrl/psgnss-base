# UM980 receiver driver — design

2026-10-31. The station's receiver was swapped from the u-blox ZED-X20P to an
ArduSimple simpleRTK3B Budget (Unicore UM980, firmware R4.10Build20739). The
owner wants **every feature the X20P had, on the UM980, plus GLONASS**, with the
**UBX driver left fully intact** so swapping back is a configuration change.

## Goals

1. Corrections: both mountpoints serve GPS+GLONASS+Galileo+BeiDou MSM, the
   receiver's own ephemeris, and GLONASS code-phase biases (1230).
2. Dashboard and Data History: skyplot, per-band C/N0, fix status, satellite
   counts — from Unicore logs, into the existing `telemetry` model, so the pages
   and the stored history format are unchanged.
3. Time to chrony from the receiver (the SOCK refclock), as with NAV-PVT.
4. RINEX downloads of UM980 data; archives recorded before the swap stay
   convertible.
5. Nightly integrity check fed from the UM980.
6. Settings page "apply base position" and "apply station ID" write the UM980.
7. Receiver page: a live command console plus readable panels (version, base
   mode, active outputs, configuration, masks), with the same safety model as
   the UBX page: verified change → pending → Keep (SAVECONFIG) or auto-revert,
   every change audited.
8. UM980 becomes a fully supported board profile (installer, units, detection).

## Non-goals / anti-goals

- No change to UBX behaviour. Every existing test stays green; the X20P path is
  selected by `receiver.model = "ZED-X20P"` exactly as before.
- Never flash receiver firmware. The console refuses firmware-upgrade commands.
- Observations are never re-encoded. MSM frames pass byte for byte, except the
  existing per-mountpoint epoch-terminator bit.
- A console command must not take the station off the air by accident.

## Facts established on the live receiver

- USB is an FTDI FT230X bridge to COM1: `/dev/serial/by-id/usb-FTDI_FT230X_Basic_UART_DU0FEZXF-if00-port0`,
  115200 8N1. About 3 KB/s RTCM at 1 Hz, so there is headroom.
- MSM station ID (DF003) is **1** (`MODE BASE 1 TIME 60`); `station.station_id = 1`
  matches it. Both must change together, as before.
- The receiver emits RTCM ephemeris itself: 1019, 1020, 1042, 1044, 1045, 1046,
  **one message per second per type, rotating through satellites**.
- MSM7 1077/1087/1097/1107/1117/1127, MSM4 1074/1084/1094/1124, and 1230 every
  10 s are enabled and saved.
- PPS is enabled (`CONFIG PPS ENABLE GPS POSITIVE 500000 1000 0 0`) and wired
  to GPIO 18, verified with ppstest.
- Binary frames: sync `AA 44 B5`, 24-byte header (msg ID u16 @4, data length
  u16 @6, time status u8 @9, week u16 @10, ms-of-week u32 @12, leap s u8 @21,
  output delay ms u16 @22), then data, then CRC32 over header+data. The CRC is
  NovAtel's (reflected 0xEDB88320, init 0, no final xor), verified against a
  live VERSIONA.
- ASCII logs: `#NAME,hdr...;data*xxxxxxxx\r\n`, CRC32 over the bytes between
  `#` and `*`. Some (UNILOGLIST) span several lines before the `*`.
- Command acknowledgements: `$command,<cmd>,response: OK*XX` (NMEA XOR
  checksum). Configuration queries answer with `$CONFIG,...*XX` lines.
- `UNLOG` with no message, `UNLOG COM1`, `FRESET`, `CONFIG COM1 <baud>` and
  `MODE ROVER` all take the station off the air. A `MODE BASE` position more
  than 300 m from the receiver's own solution suppresses all RTCM.

Reference: Unicore "Reference Commands Manual for N4 High Precision Products"
R1.14 (not committed; the owner's copy is in E:\psgnss\.ref). The captured
samples used by the decoder tests are in `internal/telemetry/testdata/`.

## Design

**Driver selection.** `boardprofile` gains `DriverUnicore = "unicore-cli"`;
the UM980 profile becomes `Implemented: true`. Everything that behaves
differently asks the resolved profile, never the model string.

**Hub.** The scanner learns two more frame kinds: `ProtoUnicore` (binary,
`Frame.Type` = message ID, CRC32-validated) and `ProtoUnicoreText` (the ASCII
`#...*crc` logs, CRC-validated). `$command`/`$CONFIG` lines already frame as
NMEA. `Filter` can select a set of protocols. `Hub.Control` takes the
protocols a control session needs, so the UBX session keeps getting UBX only,
and the Unicore session gets NMEA plus UnicoreText.

**Telemetry.** SATSINFOB (2124) → `[]Sat` and `[]Signal`, with Unicore system
and frequency identifiers mapped to u-blox gnssId/sigId so the UI's band
labels work unchanged. BESTNAVB → fix type (FIXEDPOS → 5, "time only (fixed
base)") and satellites used. The leap seconds, week and time of week for
`GPSTime` come from the binary header. BESTSATB supplies "used" flags if
SATSINFO lacks them. The receiver must have `SATSINFOB 1` and `BESTNAVB 1`
enabled; the driver ensures this at startup.

**Time sync.** The BESTNAVB header time (week + ms, FINE time status), corrected
by the header output delay, is the sample time. Otherwise it is the same Sender
and the same rejection rules.

**Ephemeris.** For a profile whose receiver emits RTCM ephemeris, the listed
ephemeris types are filtered from the stream with **no decimation**, instead of
being synthesised from UBX. The synthesiser stays and is used by the UBX
profile.

**RINEX.** For UM980 data the RTCM archive (MSM7 + ephemeris, all in one file)
is converted with `convbin -r rtcm3`, with `-tr` as two arguments. The archive
content decides the format per file, so pre-swap UBX files keep converting.

**Integrity.** rtkrcv's rover input is `rtcm3` from a private hub feed of MSM7
plus ephemeris when the profile is Unicore. The VRS base input is unchanged.

**Receiver control.** `internal/unicore.Session` sends a command and collects
its ack plus any following text log until it is quiet or times out. The web API
classifies each console command:

- *query*: read-only, run directly;
- *change*: snapshot (`CONFIG`, `UNILOGLIST`, `MODE`, `MASK`) → apply →
  re-read and verify → pending. Keep = `SAVECONFIG`. Revert, or no Keep within
  `receiver.revert_timeout`, re-issues the snapshot differences;
- *dangerous* (the list above): needs an explicit confirm flag;
- *refused*: firmware upgrade.

Every change and command goes to `receiver_config_log`. The page shows panels
built from the same queries, the console with history, and the existing
pending banner.

## Left to the builder

Exact SATSINFO field offsets (cross-check the binary against the ASCII in the
capture), the frequency-ID mapping table, UI layout details, and test fixtures.
