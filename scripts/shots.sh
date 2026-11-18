#!/bin/sh
# Retakes every screenshot the site shows, with Linssi itself, and writes the
# demo's data (site/shots/demo.js). Run from the repo root after changing the
# site or the demo pages:  sh scripts/shots.sh
set -eu

cd "$(dirname "$0")/../site"
bin=$(mktemp -d)/linssi
(cd .. && go build -o "$bin" .)

python3 -I -m http.server 3000 --bind 127.0.0.1 --directory . >/dev/null 2>&1 &
server=$!
trap 'kill $server' EXIT
sleep 1

url=localhost:3000/demo/billing/
mkdir -p shots/tiles
rm -f shots/tiles/*

"$bin" --json --scale 1 --format webp -o shots/demo-viewport.webp "$url" >shots/viewport.json
"$bin" --json --full --scale 1 --format webp -o shots/demo-full.webp "$url" >shots/full.json
"$bin" --json --selector '#plan' --padding 24 -o shots/demo-element.png "$url" >shots/element.json

python3 -I - <<'PY'
import json
modes = {
    "viewport": ("--scale 1 localhost:3000/demo/billing/", "demo-viewport.webp", "The demo billing page's first screen, captured by Linssi."),
    "full": ("--full --scale 1 localhost:3000/demo/billing/", "demo-full.webp", "The whole demo billing page, top to bottom, captured by Linssi."),
    "element": ("--selector '#plan' localhost:3000/demo/billing/", "demo-element.png", "The billing plan card alone, framed by Linssi's element capture."),
}
out = {}
for mode, (args, src, alt) in modes.items():
    r = json.load(open(f"shots/{mode}.json"))
    assert r["status"] == "ok", r
    scale = r["scale"]
    out[mode] = {
        "args": args, "src": "shots/" + src, "alt": alt,
        "w": round(r["css_width"] * scale), "h": round(r["css_height"] * scale),
        "report": {k: r[k] for k in ("css_width", "css_height", "scale", "mode", "http_status", "elapsed_ms", "tiles")},
    }
with open("shots/demo.js", "w") as f:
    f.write("// Written by scripts/shots.sh from real Linssi captures of the demo page.\n")
    f.write("window.LINSSI_DEMO = " + json.dumps(out, indent=2) + ";\n")
PY
rm shots/viewport.json shots/full.json shots/element.json

# Tiles of this very page. The page shows its own tiles, so take them twice
# to let the picture settle.
for _ in 1 2; do
  rm -f shots/tiles/*
  "$bin" --full --max-pixels 1150000 --format webp -o shots/tiles/page.webp localhost:3000/
done
