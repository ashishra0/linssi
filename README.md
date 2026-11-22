# linssi

A camera for agents. One command or MCP call returns one screenshot you can trust: the real page, logged in, fully rendered, and sized so the model can read it.

```
linssi example.com                          # 1440×900 @2x → example.com.png
linssi --full --dark tailwindcss.com        # full page, dark color scheme
linssi --selector '#hero' --padding 24 app.dev
linssi --connect 9222 localhost:3000/app    # through 'linssi browser', signed in
linssi --storage-state auth.json app.dev    # cookies + localStorage from a file
linssi -H 'Authorization: Bearer TOKEN' localhost:8080/admin
linssi --full --max-pixels 1150000 app.dev  # readable tiles sized for a model
linssi --freeze --hide '#cookie-banner' --mask '.timestamp' app.dev
cat urls.txt | linssi - -o shots/           # concurrent batch
```

## Install

```
curl -fsSL https://linssi.app/install | sh
# or
go install github.com/ashishra0/linssi/cmd/linssi@latest
```

Needs a Chrome-family browser (Chrome, Chromium, Edge or Brave). Nothing else.

## Give your agent eyes

```
claude mcp add linssi -- linssi mcp        # Claude Code
codex mcp add linssi -- linssi mcp         # Codex
```

Cursor and any other MCP client: command `linssi`, args `["mcp"]`. For Cursor, add this to `~/.cursor/mcp.json`:

```json
{ "mcpServers": { "linssi": { "command": "linssi", "args": ["mcp"] } } }
```

Add `"--connect", "9222"` to the args to capture signed-in pages (see below). If the installer said its folder isn't on your PATH, use the full path it printed as the command.

The server has one tool, `capture`. It returns the image inline, or ordered tiles for tall pages, plus a report:

```json
{"status":"ok","url":"http://localhost:3000/billing","mode":"element","selector":"#plan","css_width":480,"css_height":312,"scale":2,"tiles":1,
 "final_url":"http://localhost:3000/billing","http_status":200,"title":"Billing","warnings":[],"elapsed_ms":412}
```

MCP captures default to 1.15 MP per image, about what Claude keeps before it downscales. Pass `max_pixels: 0` to turn the budget off.

## Logged-in pages

1. **A browser you sign in to once.** Run `linssi browser`. It opens Chrome with a profile of its own and the DevTools port 9222. Sign in to your apps in that window; the profile keeps the logins. Then capture through it:

   ```
   linssi --connect 9222 localhost:3000/app
   claude mcp add linssi -- linssi mcp --connect 9222
   ```

   Linssi opens its own tab, captures, and closes that tab only; the browser keeps running. Why a separate profile: Chrome 136 and later refuse remote debugging on your everyday profile, so `--connect` can't reach the Chrome you browse with. Any Chrome started with `--remote-debugging-port=PORT --user-data-dir=DIR` works too.
2. **Saved session.** `--storage-state auth.json` takes a [Playwright storage state](https://playwright.dev/docs/auth) or a cookie-array export from a browser extension.
3. **Headers.** `-H 'Name: value'`, repeatable. Every request the page makes gets them, third-party ones too, so use a scoped token.

## Warnings

The JSON report (`--json`) and every MCP result carry the final URL, HTTP status and title. The normal CLI output prints the warnings, and the final URL when the page redirected. Warnings cover:

- HTTP 4xx and 5xx
- a redirect to what looks like a login page
- a sign-in or sign-up screen shown in place, with no redirect: a password field, a "Sign in with…" or "Continue with…" button, or the Apple or Google sign-in button on the first screen
- a blank page
- up to five console errors
- a request that never finished (captured anyway after 5 s)
- the page still changing, images still loading, or animations still running after the settle limits
- downscaling below 0.75x (small text may be hard to read)
- truncation past 8 tiles

## Develop

```
go test ./...          # browser tests use the Chrome found on this machine
go build ./cmd/linssi
```

Releases: push a `v*` tag; `.github/workflows/release.yml` builds macOS, Linux and Windows binaries, which `site/install.sh` (served at linssi.app/install) downloads.

Website: `site/` is a static site, deployed to Cloudflare at linssi.app with `npx wrangler deploy`. `go run scripts/art.go` redraws the hero art, and `sh scripts/shots.sh` retakes every screenshot on the page with Linssi itself.

## Not in scope

Clicking, typing and scripted flows. Use a browser-automation tool for those, then capture with Linssi.
