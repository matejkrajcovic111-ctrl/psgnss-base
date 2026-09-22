# uicheck

Renders the web interface **from the working tree** against a live station, in
a headless browser, so a change can be looked at instead of imagined.

Two bugs shipped because nobody did that, and both were found the first time
someone did: a map drawn into a container 0 pixels high — Leaflet initialised,
fetched its tiles and drew them into nothing, with no error anywhere — and a
log row using a four-column grid for three children, wasting 384 pixels beside
every message.

## Use

```sh
# assets from ../../internal/web/assets, /api and /tiles proxied to a station
PSGNSS_URL=http://station:8090 python3 serve.py 8791

chromium --headless --no-sandbox --disable-gpu --window-size=1600,1200 \
  --virtual-time-budget=16000 --screenshot=out.png http://127.0.0.1:8791/
```

With `PROBE=<file.js>` the script is injected before `</body>`:

| Probe | What it does |
|---|---|
| `probe_layout.js` | Panel geometry and hit-testing; appends a `<pre id="probe">` to read with `--dump-dom` |
| `probe_gestures.js` | Drives the panel grip and all eight resize handles with synthetic events and reports what the layout did |
| `probe_tab.js` | Clicks the tab named in the URL fragment before the screenshot, since the UI keeps the current tab in React state rather than the URL |

Other environment variables:

- `PSGNSS_SESSION=<token>` — carries a session cookie on proxied API calls, for
  pages behind a login.
- `PSGNSS_DEMO=1` — rounds the station position to the two decimal places a
  public sourcetable already advertises, and renames the station. For
  screenshots that go into documentation.

## Two things that will mislead you

**Read the DOM asynchronously.** React commits after the event, so a
measurement taken synchronously after dispatching one reports the state
*before* the gesture. That looked exactly like a broken reorder for a while.

**Transitions do not tick under `--virtual-time-budget`,** so an opacity read
always catches the start value. Disable the transition in the probe when what
is being tested is which selector wins.
