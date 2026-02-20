"""Simple task agent — demonstrates secrets, workspace, and logging conventions."""
import os
import json
import sys
from datetime import datetime, timezone

def log(level, msg, **kwargs):
    entry = {"level": level, "msg": msg, "ts": datetime.now(timezone.utc).isoformat(), **kwargs}
    print(json.dumps(entry), flush=True)

def main():
    log("info", "agent starting")

    # Read a secret from environment
    greeting = os.environ.get("GREETING", "Hello from AegisVM")
    log("info", "greeting loaded", greeting=greeting)

    # Write output to workspace
    output_dir = "/workspace/output"
    os.makedirs(output_dir, exist_ok=True)

    output_file = os.path.join(output_dir, "result.txt")
    with open(output_file, "w") as f:
        f.write(f"{greeting}\n")
        f.write(f"Generated at: {datetime.now(timezone.utc).isoformat()}\n")

    log("info", "output written", path=output_file)
    log("info", "agent finished")

if __name__ == "__main__":
    main()
