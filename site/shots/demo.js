// Written by site/shots.sh from real Linssi captures of the demo page.
window.LINSSI_DEMO = {
  "viewport": {
    "args": "--scale 1 localhost:3000/demo/billing/",
    "src": "shots/demo-viewport.webp",
    "alt": "The demo billing page's first screen, captured by Linssi.",
    "w": 1440,
    "h": 900,
    "report": {
      "css_width": 1440,
      "css_height": 900,
      "scale": 1,
      "mode": "viewport",
      "http_status": 200,
      "elapsed_ms": 1080,
      "tiles": 1
    }
  },
  "full": {
    "args": "--full --scale 1 localhost:3000/demo/billing/",
    "src": "shots/demo-full.webp",
    "alt": "The whole demo billing page, top to bottom, captured by Linssi.",
    "w": 1440,
    "h": 1194,
    "report": {
      "css_width": 1440,
      "css_height": 1194,
      "scale": 1,
      "mode": "full_page",
      "http_status": 200,
      "elapsed_ms": 1206,
      "tiles": 1
    }
  },
  "element": {
    "args": "--selector '#plan' localhost:3000/demo/billing/",
    "src": "shots/demo-element.png",
    "alt": "The billing plan card alone, framed by Linssi's element capture.",
    "w": 1744,
    "h": 510,
    "report": {
      "css_width": 872,
      "css_height": 255,
      "scale": 2,
      "mode": "element",
      "http_status": 200,
      "elapsed_ms": 884,
      "tiles": 1
    }
  }
};
