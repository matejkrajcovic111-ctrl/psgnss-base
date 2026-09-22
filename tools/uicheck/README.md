# uicheck

Renders the web interface from the working tree against a live station, in a
headless browser, so you can look at a change instead of imagining it.

Two bugs shipped because nobody did that, and both were found the first time
somebody did. One was a map drawn into a container zero pixels high: Leaflet
initialised, fetched its tiles and drew them into nothing, with no error
anywhere. The other was a log row using a four-column grid for three children,
wasting 384 pixels beside every message.

## Use

```sh
# assets from ../../internal/web/assets, /api and /tiles proxied to a station
PSGNSS_URL=http://station:8090 python3 serve.py 8791

chromium --headless --no-sandbox --disable-gpu --window-size=1600,1200 \
  --virtual-time-budget=16000 --screenshot=out.png http://127.0.0.1:8791/
```

Set `PROBE=<file.js>` and that script gets injected before `</body>`:

| Probe | What it does |
|---|---|
| `probe_layout.js` | Panel geometry and hit-testing. Appends a `<pre id="probe">` you read back with `--dump-dom` |
| `probe_gestures.js` | Drives the panel grip and all eight resize handles with synthetic events, and reports what the layout did |
| `probe_tab.js` | Clicks the tab named in the URL fragment before the screenshot, since the UI keeps the current tab in React state rather than in the URL |

Other environment variables:

- `PSGNSS_SESSION=<token>` carries a session cookie on proxied API calls, for
  pages behind a login.
- `PSGNSS_DEMO=1` rounds the station position to the two decimal places a
  public sourcetable already advertises and renames the station, for
  screenshots that end up in documentation.

## Two things that will catch you out

Read the DOM asynchronously. React commits after the event, so a measurement
taken immediately after dispatching one reports the state *before* the gesture.
That looked exactly like a broken reorder for a while.

Transitions don't tick under `--virtual-time-budget`, so an opacity read always
catches the start value. Disable the transition in the probe when what you're
testing is which selector wins.
