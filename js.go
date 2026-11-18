package linssi

import (
	"encoding/json"
	"fmt"
)

// settleJS waits for what a person would wait for: fonts, the DOM to stop
// changing, nearby images (all images when whole is true) and finite entrance
// animations, each capped at 3s. Infinite animations never finish, so they are skipped. It reports what
// was still unsettled when the caps ran out.
const settleJS = `async (whole) => {
  const frames = () => new Promise(r => requestAnimationFrame(() => requestAnimationFrame(r)));
  const after = (ms, v) => new Promise(r => setTimeout(() => r(v), ms));
  if (document.fonts) await Promise.race([document.fonts.ready, after(3000)]).catch(() => {});
  // Let client-side rendering finish: 300ms since the last DOM change (tracked
  // from document start by changeTrackerJS), at most 2s.
  let quiet = false;
  for (const end = performance.now() + 2000; performance.now() < end; await after(50)) {
    if (performance.now() - (window.__linssiLastChange ?? 0) >= 300) { quiet = true; break; }
  }
  const near = img => {
    if (whole) return true;
    const b = img.getBoundingClientRect();
    return b.bottom > -innerHeight / 2 && b.top < innerHeight * 1.5;
  };
  const pending = [...document.images].filter(img => !img.complete && near(img));
  await Promise.race([
    Promise.all(pending.map(img => new Promise(r => {
      img.addEventListener('load', r, { once: true });
      img.addEventListener('error', r, { once: true });
    }))),
    after(3000),
  ]);
  const images = pending.filter(img => !img.complete).length;
  await frames();
  const running = document.getAnimations().filter(a => {
    try { return a.playState === 'running' && a.effect.getComputedTiming().iterations !== Infinity; }
    catch { return false; }
  });
  const animations = await Promise.race([
    Promise.all(running.map(a => a.finished.catch(() => {}))).then(() => false),
    after(3000, true),
  ]);
  await frames();
  return { images, animations, quiet };
}`

// changeTrackerJS runs before any page script and records the time of the
// latest DOM change, so settle can tell a still page from a busy one at once.
const changeTrackerJS = `(() => {
  window.__linssiLastChange = performance.now();
  new MutationObserver(() => { window.__linssiLastChange = performance.now(); })
    .observe(document, { subtree: true, childList: true, attributes: true, characterData: true });
})()`

// scrollJS walks down the page one screen at a time so lazy content loads,
// then returns to the top.
const scrollJS = `(async () => {
  const height = () => Math.max(document.body?.scrollHeight ?? 0, document.documentElement.scrollHeight);
  for (let y = 0, steps = 0; y < height() && steps < 400; y += Math.max(innerHeight, 200), steps++) {
    scrollTo(0, y);
    await new Promise(r => setTimeout(r, 50));
  }
  scrollTo(0, 0);
  await new Promise(r => setTimeout(r, 150));
  await new Promise(r => requestAnimationFrame(() => requestAnimationFrame(r)));
})()`

// probeJS reads the page state the report needs.
const probeJS = `(() => {
  const nav = performance.getEntriesByType('navigation')[0];
  const body = document.body;
  const text = (body?.innerText ?? '').trim().length;
  const media = document.querySelector('img, svg, canvas, video, picture, iframe');
  return {
    url: location.href,
    status: nav && nav.responseStatus ? nav.responseStatus : null,
    title: document.title,
    blank: !body || (text === 0 && !media),
    scrollX, scrollY,
    docHeight: Math.max(body?.scrollHeight ?? 0, document.documentElement.scrollHeight),
    innerWidth, innerHeight,
  };
})()`

func quote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// waitForJS polls until selector matches, then optionally centers it on screen.
func waitForJS(selector string, budgetMS int64, scroll bool) string {
	return fmt.Sprintf(`(async () => {
  const selector = %s, deadline = Date.now() + %d;
  const find = () => {
    try { return document.querySelector(selector); }
    catch { throw new Error('invalid selector: ' + selector); }
  };
  let el;
  while (!(el = find())) {
    if (Date.now() > deadline) throw new Error('selector never appeared: ' + selector);
    await new Promise(r => setTimeout(r, 100));
  }
  if (%t) {
    el.scrollIntoView({ block: 'center', inline: 'center' });
    await new Promise(r => requestAnimationFrame(() => requestAnimationFrame(r)));
  }
})()`, quote(selector), budgetMS, scroll)
}

// boundsJS returns the first match's box in document coordinates.
func boundsJS(selector string) string {
	return fmt.Sprintf(`(() => {
  const selector = %s;
  let el;
  try { el = document.querySelector(selector); }
  catch { throw new Error('invalid selector: ' + selector); }
  if (!el) throw new Error('selector disappeared: ' + selector);
  const r = el.getBoundingClientRect();
  if (!(r.width > 0 && r.height > 0)) throw new Error('element has no rendered size: ' + selector);
  const d = document.documentElement, b = document.body;
  return {
    X: r.left + scrollX, Y: r.top + scrollY, W: r.width, H: r.height,
    DocW: Math.max(b?.scrollWidth ?? 0, d.scrollWidth, d.clientWidth),
    DocH: Math.max(b?.scrollHeight ?? 0, d.scrollHeight, d.clientHeight),
  };
})()`, quote(selector))
}

// stabilizeJS makes repeat captures match: it finishes entrance animations,
// parks loops at their first frame, stops video and the caret, hides some
// elements and paints others flat gray.
func stabilizeJS(freeze bool, hide, mask []string) string {
	h, _ := json.Marshal(append([]string{}, hide...))
	m, _ := json.Marshal(append([]string{}, mask...))
	return fmt.Sprintf(`(async () => {
  const freeze = %t, hide = %s, mask = %s;
  for (const s of [...hide, ...mask]) {
    try { document.querySelectorAll(s); }
    catch { throw new Error('invalid selector: ' + s); }
  }
  let css = '';
  if (freeze) {
    css += '*,*::before,*::after{animation-play-state:paused!important;transition:none!important;caret-color:transparent!important;scroll-behavior:auto!important}';
    for (const a of document.getAnimations()) {
      try {
        if (a.effect.getComputedTiming().iterations === Infinity) { a.pause(); a.currentTime = 0; }
        else a.finish();
      } catch {}
    }
    for (const v of document.querySelectorAll('video')) { try { v.pause(); v.currentTime = 0; } catch {} }
  }
  // :is() keeps a comma list like ".a, .b" together as one selector.
  for (const s of hide) css += ':is(' + s + '){visibility:hidden!important}';
  for (const s of mask) css += ':is(' + s + '){background:#888!important;color:transparent!important;border-color:#888!important;box-shadow:none!important}:is(' + s + ') *{visibility:hidden!important}';
  const style = document.createElement('style');
  style.textContent = css;
  document.documentElement.appendChild(style);
  await new Promise(r => requestAnimationFrame(() => requestAnimationFrame(r)));
})()`, freeze, h, m)
}
