"""Simple HTTP server — demonstrates serve mode, secrets, and workspace."""
import os
import json
from http.server import HTTPServer, BaseHTTPRequestHandler
from datetime import datetime, timezone

def log(level, msg, **kwargs):
    entry = {"level": level, "msg": msg, "ts": datetime.now(timezone.utc).isoformat(), **kwargs}
    print(json.dumps(entry), flush=True)

class Handler(BaseHTTPRequestHandler):
    def do_GET(self):
        api_key = os.environ.get("API_KEY", "")
        has_key = "yes" if api_key else "no"

        # List workspace contents
        workspace = "/workspace"
        files = []
        if os.path.isdir(workspace):
            files = os.listdir(workspace)

        body = f"""<html>
<body>
<h1>AegisVM HTTP Server</h1>
<p>API_KEY configured: {has_key}</p>
<p>Workspace files: {', '.join(files) if files else '(empty)'}</p>
<p>Time: {datetime.now(timezone.utc).isoformat()}</p>
</body>
</html>"""

        self.send_response(200)
        self.send_header("Content-Type", "text/html")
        self.end_headers()
        self.wfile.write(body.encode())
        log("info", "request served", path=self.path, method="GET")

    def log_message(self, format, *args):
        pass  # suppress default logging

if __name__ == "__main__":
    log("info", "starting server", port=80)
    server = HTTPServer(("0.0.0.0", 80), Handler)
    server.serve_forever()
