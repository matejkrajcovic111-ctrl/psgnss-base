#!/usr/bin/env python3
"""Serve the repo's web assets locally, proxying /api and /tiles to a station.

Dev harness only: it lets a headless browser render the real dashboard from the
working tree, so a CSS or layout change can be checked before it is deployed.
"""
import http.server, socketserver, urllib.request, json, os, sys

ASSETS = os.path.join(os.path.dirname(os.path.abspath(__file__)),
                      "..", "..", "internal", "web", "assets")
PI = os.environ.get("PSGNSS_URL", "http://127.0.0.1:8090")
PROBE = os.environ.get("PROBE", "")
# Screenshots for the documentation must not publish a station's surveyed
# position. Demo mode rounds it to the two decimal places the public
# sourcetable already advertises, and renames the station.
DEMO = os.environ.get("PSGNSS_DEMO", "")
SESSION = os.environ.get("PSGNSS_SESSION", "")

class H(http.server.SimpleHTTPRequestHandler):
    def __init__(self, *a, **k):
        super().__init__(*a, directory=ASSETS, **k)
    def log_message(self, *a): pass
    def do_GET(self):
        p = self.path.split("?")[0]
        if p.startswith("/api/") or p.startswith("/tiles/"):
            try:
                req = urllib.request.Request(PI + self.path)
                if SESSION:
                    # An administrator page cannot be rendered without a
                    # session, and the browser has no way to get one from a
                    # locally served page. The proxy carries it instead.
                    req.add_header("Cookie", "psgnss_session=" + SESSION)
                with urllib.request.urlopen(req, timeout=10) as r:
                    body, ct = r.read(), r.headers.get("Content-Type", "application/json")
                if DEMO and "json" in ct:
                    body = demo(body)
                self.send_response(200); self.send_header("Content-Type", ct)
                self.send_header("Content-Length", str(len(body))); self.end_headers()
                self.wfile.write(body)
            except Exception as e:
                self.send_error(502, str(e))
            return
        if p in ("/", "/index.html"):
            html = open(os.path.join(ASSETS, "index.html")).read()
            if PROBE:
                html = html.replace("</body>", '<script src="/probe.js"></script></body>')
            b = html.encode()
            self.send_response(200); self.send_header("Content-Type", "text/html")
            self.send_header("Content-Length", str(len(b))); self.end_headers()
            self.wfile.write(b); return
        if p == "/probe.js":
            b = open(PROBE, "rb").read()
            self.send_response(200); self.send_header("Content-Type", "text/javascript")
            self.send_header("Content-Length", str(len(b))); self.end_headers()
            self.wfile.write(b); return
        super().do_GET()

def demo(body):
    """Round the position and rename the station, for documentation shots."""
    try:
        doc = json.loads(body)
    except Exception:
        return body
    if isinstance(doc, dict):
        pos = doc.get("station_position")
        if isinstance(pos, dict):
            for k in ("latitude", "longitude"):
                if isinstance(pos.get(k), (int, float)):
                    pos[k] = round(pos[k], 2)
            if isinstance(pos.get("height"), (int, float)):
                pos["height"] = round(pos["height"])
        if doc.get("station"):
            doc["station"] = "Example"
    return json.dumps(doc).encode()


socketserver.TCPServer.allow_reuse_address = True
with socketserver.TCPServer(("127.0.0.1", int(sys.argv[1])), H) as s:
    s.serve_forever()
