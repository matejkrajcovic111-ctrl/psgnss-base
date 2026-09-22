# Hardware notes

Things about this hardware that aren't in a datasheet, or are and are wrong.
All of it was measured on a running station, and several of them cost a day
before they were understood.

## The receiver

**It can't emit RTCM ephemeris.** The u-blox ZED-X20P's key set has no 1019,
1020, 1042, 1045 or 1046. That was checked against the full 800-key
`CFG-MSGOUT` dump rather than assumed. PSGNSS synthesises them from
`RXM-SFRBX` instead, so that's where navigation messages on a mountpoint come
from.

**GLONASS isn't supported by this firmware.** `MON-VER` reports
`GPS;GAL;BDS / SBAS;QZSS / NAVIC`. You can enable 1084 and 1087 on USB and
they'll emit nothing. Don't try to fix it, and don't put GLONASS in a
mountpoint's `nav_system`: advertising a constellation you don't carry is worse
than not offering it.

**It emits MSM4 and MSM7 at the same time,** which is why a mountpoint can be a
pure filter.

**It emits 1005 and never 1006, 1008 or 1033.** PSGNSS generates those three
from configuration.

**`CFG-SIGNAL` writes reset the GNSS subsystem.** u-blox says to wait for the
ACK and then 0.5 s before the next command. PSGNSS waits a full second after
any batch touching that group and retries the read-back. Measured during a live
enable-and-revert: epoch age stayed at 0–1 s and the hub dropped nothing.

**`VALSET` takes at most 64 keys.** Bigger transactions get chunked on write,
persist and revert.

**`UBX-MON-SYS` `tempValue` is documented as unsupported** on HPG 2.00 and
always reads 0, so there's no receiver temperature to show. `errorCount` climbs
steadily on a perfectly healthy receiver and nobody here knows what it counts.

**RAM and Flash configuration are byte-identical** once committed, so settings
survive a power cycle.

## Reference station ID

The generated 1006/1008/1033 and the receiver's own 1077/1097/1127 have to
carry the same reference station ID. When they don't, a rover receives a
healthy-looking stream, tracks satellites and never fixes, because it can't
associate the observations with a base position.

This receiver family stamps `DF003 = 0` out of the box, so 0 is the answer
unless you're also writing `CFG-RTCM`. Change one side without the other and
you get a station that looks perfect and serves nothing usable, which is an
unpleasant afternoon.

## Epoch termination

The receiver marks the end of each epoch once, on the last message it sends. A
mountpoint serving a subset filters that marker away, and the rover then can't
close the epoch until the next one begins. That's a full extra second of
correction age, and the only symptom is that performance is worse than it
should be. Each mountpoint marks its own last observation message instead.

## RINEX conversion (`convbin`)

`-f` must be 4 on this receiver:

| `-f` | Result |
|---|---|
| 2 | GPS L1/L2 and Galileo E1 only. BeiDou missing entirely. |
| 3 | Adds GPS L5, Galileo E5a, BeiDou B2a |
| 4 | Adds Galileo E6 and BeiDou B3I, which is everything |
| 5+ | Identical to 4 |

A tool that hardcodes 2, which is an F9P-era default, produces RINEX with no
BeiDou in it at all and says nothing. PSGNSS refuses to start if the configured
count would lose signals the receiver tracks.

**BeiDou B1C never shows up in RINEX at any setting.** That's an RTKLIB mapping
limitation, not something you've configured wrong.

**`-ts` and `-te` take the date and the time as two separate arguments.**
Passing `"2026/09/14 09:00:00"` as one makes convbin swallow the next flag and
silently convert the whole file. You won't notice if you only ever ask for
whole days.

**convbin makes an unpredictable number of passes** over a file and reports
progress as a timestamp on stderr. PSGNSS computes progress monotonically,
because a bar that jumps backwards is worse than one that stalls.

For the version, use `convbin --version`. With no arguments it just prints "no
input file". `psgnssd --dependencies` reports whatever is installed.

## RINEX navigation data

RTCM carries observations and no navigation data, so a RINEX file converted
from an RTCM-only archive gets a `.obs` and no `.nav`. The navigation archive
records `RXM-SFRBX` and `RXM-RAWX`: the subframes carry the broadcast
ephemeris, and the raw measurement epochs carry the receiver time RTKLIB needs
to date them. An SFRBX-only file looks fine and won't convert.

## systemd

**`ProtectSystem=strict` blocks writes to the archive mount** unless its path
is in `ReadWritePaths`. Without that, every write to a healthy, read-write
share fails with "read-only file system", and the only sign is an empty archive
and a line in the log. This cost a full day of recording once.

**A mount unit's filename has to be the systemd-escaped mount path.**
`/mnt/gnssraw` must be `mnt-gnssraw.mount`. Call it anything else and the unit
is inert: never read, share never mounted, nothing complains. `psgnssd --setup`
derives the name using the same rules systemd does, and the tests check it
against `systemd-escape` itself rather than against someone's reading of the
manual.

## Operational

**Filenames are generated in UTC.** Local time gives you an hour that shifts
twice a year and breaks continuity with everything already recorded.

**Shutdown isn't file completion.** Treating a stop as the end of a file throws
away the spool copy and the resume state, which fragments the day on every
restart.

**A long `ssh` command that hits a local timeout leaves the remote process
running.** This has produced a phantom "performance regression" more than once,
where the culprit was an orphaned helper spinning at 81% of a core for the best
part of an hour. Check what's actually running before you conclude anything.

**`psgnssd` uses about 0.35% of one core.** If the machine looks busy, it's
something else.
