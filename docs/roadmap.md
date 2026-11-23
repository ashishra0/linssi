# Linssi roadmap: the next four weeks

## Goal

Make Linssi the default way agents see a page, by being the screenshot tool whose captures can be trusted: a capture is either right, or it says why it isn't.

Don't compete on raw speed or breadth; Iris is faster and Playwright does far more. Compete on **trust** (no silently wrong images), **token cost** (agents pay for every image) and **signed-in pages**.

The evidence so far, from 2026-11-23, with Iris 0.4.1 and Linssi on the same Chrome for Testing:

| Page | Iris | Linssi |
|---|---|---|
| access.mymind.com, signed out | blank white image, reported `ok` (2 of 2 runs); needed `--wait 3000` | real page plus a "sign-in or sign-up screen" warning |
| Wikipedia "Lighthouse", `--full` | 4.1 s, one image at 1x | 10.9 s, 2x in 2 tiles, warned "8 image(s) still loading" |
| example.com, 404, bad domain | same as Linssi | same as Iris |

## How we measure

- **Silent failure rate:** a wrong image reported as `ok`, across the benchmark. Target: under 1%.
- **Latency:** typical warm capture under 1 s, slowest 1-in-20 under 4 s.
- **Tokens per useful answer:** image plus text an agent needs to answer a question about a page.
- No telemetry. Adoption is tracked through release downloads, Homebrew installs and stars.

## Week 1: the reliability benchmark

The benchmark is the core of the plan: it's the test suite, the roadmap and the marketing at once.

1. Build a corpus of pages that break screenshot tools. Start with about 50 and grow to about 200. Each case records its URL (or a local fixture), the expected outcome (a real page, or which warning it must raise), and why it's hard. Categories:
   - sign-in walls, with and without a redirect (mymind is case #1)
   - fade-in and entrance animations
   - cookie and consent banners
   - lazy images, single-page apps still loading, endless feeds
   - error pages that return HTTP 200, bot challenges (Cloudflare "Just a moment…")
   - very tall pages, dark mode, mobile presets
2. Prefer local fixtures (an `httptest` site, like `linssi_test.go`) so results are repeatable. Keep a small set of live sites for realism and mark them as flaky-by-nature.
3. Write a runner that captures every case with each tool and grades the result: right page, correct warning, silent failure or error. Tools: Linssi, Iris, Playwright MCP. Chrome DevTools MCP if it's cheap to add.
4. Detect silent failures with automatic checks: near-blank images, the expected text present or absent, size. Fall back to a manual label only where needed.
5. Print a table and write JSON results, then run it nightly in CI.

**Done when** one command prints the silent failure rate per tool, and the mymind blank-page case is in the corpus.

## Week 2: close the silent-failure gaps

Fix what the benchmark finds. Likely first fixes:

1. **Near-blank image check.** Warn when a capture is almost all one colour. This catches the Iris-type failure even when the DOM looks fine.
2. **Bot-challenge detection** (Cloudflare and similar interstitials).
3. **Error pages served with HTTP 200** (titles and text like "Page not found").
4. **Adaptive settling.** Capture as soon as the page is stable (no layout shifts, fonts and images done) instead of waiting out fixed limits. This cuts the 11 s tall-page time without losing correctness.
5. **`--expect-text TEXT`** (and the same MCP option): the capture fails loudly when the text isn't on the page.
6. Fix the timing-sensitive test that failed once when both test packages ran at the same time.

**Done when** Linssi's silent failure rate on the benchmark is under 1%, and a typical warm capture takes under 1 s on local fixtures.

## Week 3: `linssi login`

1. `linssi login mymind.com` opens Linssi's browser on that site and watches for the sign-in screen to disappear (using the sign-in detection from `09a99f4`). It then saves the session (`linssi session export` under the hood) and closes the browser.
2. Sessions are saved per site, and `--storage-state` can find them by domain.
3. When a saved session has expired, a capture warns "session for X expired; run linssi login X" instead of quietly capturing the sign-in page.
4. Test it end to end on a local fixture with a fake sign-in, then on mymind.

**Done when** a signed-in mymind capture works from a cold start with one `linssi login` and one capture command.

## Week 4: publish

1. Add a results page on linssi.app: the benchmark table per tool, the method, and how to submit a failing page.
2. Put the mymind side-by-side (Iris blank vs Linssi real page plus warning) on the site as the headline example.
3. Build the v2 website design into `site/index.html` (Paper file "Linssi · Website", artboard "v2 (product)"), with the Buy Me a Coffee link.
4. Add an issue template for "this page captured wrong" that asks for the `--json` report.

**Done when** linssi.app shows live benchmark numbers and the new design.

## Later: token efficiency and distribution

- Return the page text alongside the image, so agents read words cheaply and only look at pixels for layout.
- A "what changed" capture: after an edit, return only the changed area, cropped, plus a one-line summary. Diffs come back as a free feature.
- Keep a warm browser in the MCP server.
- One-step installs for Claude Code, Codex and Cursor; Homebrew, `npx`, a Docker image, MCP registry listings, and a free GitHub Action that posts screenshots on pull requests.

## Out of scope

Clicking, typing and scripted flows; that's Playwright's job. Linssi works alongside it through `--connect`. No hosted service until local use is clearly winning.
