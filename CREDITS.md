# Credits and third-party work

PSGNSS is licensed under the GNU Affero General Public License v3.0 (see
`LICENSE`). This file records outside work it derives from, what was taken, and
the obligations that come with it.

## RTKBase — https://github.com/Stefal/rtkbase

Copyright © Stéphane Péneau and the RTKBase contributors.
Licensed under the GNU Affero General Public License v3.0 — the same licence as
this project.

RTKBase is a bash-and-Flask base-station stack built around RTKLIB's `str2str`.
PSGNSS is an independent implementation in Go and shares no source code with it,
but several PSGNSS features exist because RTKBase demonstrated their value, and
the following were designed by reading RTKBase's implementation:

| PSGNSS feature | What RTKBase contributed |
|---|---|
| NTRIP push-out to external casters | The idea and the operational shape: two independent upload targets with their own credentials, mountpoint and message selection (`[ntrip_A]` / `[ntrip_B]` in `settings.conf.default`, `run_cast.sh`) |
| Base position map | The choice to show the station on a map at all, and Leaflet as an offline-capable way to do it (`web_app/templates/status.html`) |
| Time synchronisation from GNSS | The goal, taken from RTKBase's gpsd + chrony arrangement (`tools/install.sh --gpsd-chrony`). PSGNSS reaches it differently: the daemon feeds chrony a refclock directly, with no gpsd |
| RTCM output over UDP and to a serial radio link | The output set and its configuration shape (`[rtcm_udp_svr]`, `[rtcm_udp_client]`, `[rtcm_serial]`) |
| Multi-vendor receiver support | Receiver detection and the working configuration presets for the ZED-F9P, Mosaic-X5 and Unicore UM980/UM982 (`receiver_cfg/`, `tools/unicore`, `tools/septentrio`) |

Where a PSGNSS file is a direct translation of RTKBase logic or carries values
copied from its receiver configuration presets, the file says so in a comment at
the point of use, naming the upstream file.

#### RINEX presets for post-processing services

`internal/downloader/preset.go` takes its parameters from RTKBase's
`tools/convbin.sh`: RINEX version, frequency count, excluded constellations,
sampling interval and time tolerance for the IGN, NRCan, 30 s and 1 s presets.
The station header fields written alongside them (`-hm`, `-ha`, `-hr`, `-hp`,
`-hc`) follow the same script. The values are the services' requirements, which
is precisely why they were worth taking rather than guessing.

### RTCM navigation-message synthesis

The idea of serving ephemeris from the base comes from RTKBase, whose
`str2str` message lists include 1019, 1020, 1042, 1045 and 1046. The
implementation shares no code: RTKBase gets them by re-encoding the whole raw
stream through RTKLIB, while `internal/ephemeris` decodes only the broadcast
navigation subframes and leaves observations alone.

## As implemented

| Landed | PSGNSS code | Notes |
|---|---|---|
| 2026-09-20 | `internal/pushout`, `internal/caster/tap.go` | NTRIP push-out. A target publishes a local mountpoint's exact bytes instead of a per-target message list |
| 2026-09-20 | `StationMap` in `internal/web/assets/app.js`, `internal/web/tiles.go` | Base position map. Leaflet 1.9.4 is vendored (BSD-2-Clause, © Vladimir Agafonkin). Tiles are proxied and cached by the daemon so it can identify itself to the provider, as OpenStreetMap's usage policy requires; the page itself never leaves its own origin. RTKBase solves the same problem by asking for the operator's own MapTiler key |
| 2026-09-20 | `internal/timesync` | Time from the receiver to chrony, without gpsd: chrony's SOCK refclock, fed from UBX NAV-PVT |
| 2026-09-20 | `internal/rtcmout` | RTCM over UDP and out of a serial port. Unverified on hardware |

Leaflet is a separate work with its own licence, vendored unmodified at
`internal/web/assets/leaflet-1.9.4.js` and `.css`. Its BSD-2-Clause notice is in
the file header and must stay there.

Three more browser works are vendored unmodified for the same reason -- the Pi
has no internet and the tunnel should not make a visitor wait on a CDN. Each
keeps its own licence and its own notice:

| Work | Files | Licence |
|---|---|---|
| [uPlot](https://github.com/leeoniya/uPlot) 1.6.32, © Leon Sorokin | `uplot-1.6.32.min.js`, `.css` | MIT |
| [Tabler](https://github.com/tabler/tabler) 1.4.0, © The Tabler Authors, Paweł Kuna | `tabler-1.4.0.min.css` | MIT |
| [Inter](https://github.com/rsms/inter), © The Inter Project Authors | `inter-400/500/600/700.woff2` (latin subset) | SIL Open Font License 1.1 |

Tabler supplies the interface's visual language: the palette, the radii, the
type rhythm and the button, form, table and card components. This station's
tokens are handed to it through the `--tblr-*` variables in `app.css`, so its
components follow the same theme switch as everything drawn by hand.

### What the licence requires

Both projects are AGPL-3.0, so there is no compatibility problem, but the
obligations are real and they are not optional:

- **Attribution and notices stay.** This file, `LICENSE`, and the per-file
  comments must survive. Removing them is a licence violation, not tidying.
- **Derived work stays AGPL-3.0.** PSGNSS cannot be relicensed or shipped under
  a proprietary licence while it contains work derived from RTKBase.
- **Section 13 — network use.** Anyone who interacts with PSGNSS over a network,
  which includes every operator of the web UI, is entitled to the complete
  corresponding source of the running version. Publishing this repository
  satisfies that; running a modified private copy for others does not.
- **No warranty, and none implied for upstream.** Faults in PSGNSS are not
  RTKBase's, and this file is not an endorsement by its authors.

## RTKLIB — https://github.com/rtklibexplorer/RTKLIB

PSGNSS runs `convbin` for RINEX conversion and `rtkrcv` for the external
position integrity check as external processes. They are not linked in and not
modified. RTKLIB is BSD-2-Clause; rtklibexplorer's demo5 fork carries the same
terms.

## u-blox interface descriptions

`internal/receiver/keydb.json` is generated from the published u-blox HPG
2.00/2.02/2.10 interface descriptions and from pyubx2's key tables, by
`tools/ubxkeydb`. It contains configuration key identifiers and types, which are
interface facts rather than u-blox source code. Regenerate it; never hand-edit.
