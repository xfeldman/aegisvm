"""European capitals with current time — served from an Aegis microVM."""

import json
from datetime import datetime, timezone, timedelta
from http.server import HTTPServer, BaseHTTPRequestHandler

CAPITALS = [
    ("Reykjavik",  "Iceland",        0),
    ("London",     "United Kingdom", 0),
    ("Dublin",     "Ireland",        0),
    ("Lisbon",     "Portugal",       0),
    ("Paris",      "France",         1),
    ("Brussels",   "Belgium",        1),
    ("Amsterdam",  "Netherlands",    1),
    ("Berlin",     "Germany",        1),
    ("Bern",       "Switzerland",    1),
    ("Rome",       "Italy",          1),
    ("Madrid",     "Spain",          1),
    ("Vienna",     "Austria",        1),
    ("Prague",     "Czech Republic", 1),
    ("Warsaw",     "Poland",         1),
    ("Copenhagen", "Denmark",        1),
    ("Oslo",       "Norway",         1),
    ("Stockholm",  "Sweden",         1),
    ("Zagreb",     "Croatia",        1),
    ("Ljubljana",  "Slovenia",       1),
    ("Belgrade",   "Serbia",         1),
    ("Budapest",   "Hungary",        1),
    ("Bratislava", "Slovakia",       1),
    ("Helsinki",   "Finland",        2),
    ("Tallinn",    "Estonia",        2),
    ("Riga",       "Latvia",         2),
    ("Vilnius",    "Lithuania",      2),
    ("Athens",     "Greece",         2),
    ("Bucharest",  "Romania",        2),
    ("Sofia",      "Bulgaria",       2),
    ("Kyiv",       "Ukraine",        2),
    ("Chisinau",   "Moldova",        2),
    ("Ankara",     "Turkey",         3),
    ("Minsk",      "Belarus",        3),
    ("Moscow",     "Russia",         3),
]

def render_page():
    now_utc = datetime.now(timezone.utc)
    rows = []
    for city, country, offset in CAPITALS:
        local = now_utc + timedelta(hours=offset)
        sign = "+" if offset >= 0 else ""
        rows.append(
            f"<tr>"
            f"<td>{city}</td>"
            f"<td>{country}</td>"
            f"<td>UTC{sign}{offset}</td>"
            f"<td>{local.strftime('%H:%M:%S')}</td>"
            f"</tr>"
        )
    table_rows = "\n            ".join(rows)
    return f"""<!DOCTYPE html>
<html>
<head>
    <meta charset="utf-8">
    <title>European Capitals — Current Time</title>
    <meta http-equiv="refresh" content="30">
    <style>
        body {{ font-family: -apple-system, system-ui, sans-serif; margin: 40px auto; max-width: 720px; color: #333; }}
        h1 {{ font-size: 1.5em; margin-bottom: 4px; }}
        .subtitle {{ color: #888; font-size: 0.9em; margin-bottom: 24px; }}
        table {{ width: 100%; border-collapse: collapse; }}
        th {{ text-align: left; padding: 8px 12px; border-bottom: 2px solid #ddd; font-size: 0.85em; text-transform: uppercase; color: #666; }}
        td {{ padding: 6px 12px; border-bottom: 1px solid #eee; }}
        tr:hover {{ background: #f8f8f8; }}
        .footer {{ margin-top: 24px; font-size: 0.8em; color: #aaa; }}
    </style>
</head>
<body>
    <h1>European Capitals</h1>
    <p class="subtitle">Current local time — auto-refreshes every 30s</p>
    <table>
        <thead>
            <tr><th>Capital</th><th>Country</th><th>Timezone</th><th>Local Time</th></tr>
        </thead>
        <tbody>
            {table_rows}
        </tbody>
    </table>
    <p class="footer">Served from an Aegis microVM at {now_utc.strftime('%Y-%m-%d %H:%M:%S')} UTC</p>
</body>
</html>"""


def log(level, msg, **kw):
    entry = {"level": level, "msg": msg, "ts": datetime.now(timezone.utc).isoformat(), **kw}
    print(json.dumps(entry), flush=True)


class Handler(BaseHTTPRequestHandler):
    def do_GET(self):
        html = render_page()
        self.send_response(200)
        self.send_header("Content-Type", "text/html; charset=utf-8")
        self.end_headers()
        self.wfile.write(html.encode())
        log("info", "request served", path=self.path)

    def log_message(self, fmt, *args):
        pass


if __name__ == "__main__":
    log("info", "starting europe-capitals server", port=80)
    HTTPServer(("0.0.0.0", 80), Handler).serve_forever()
