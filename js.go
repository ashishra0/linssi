package linssi

import (
	"encoding/json"
	"fmt"
)

// settleJS waits for what a person would wait for: fonts, then until the page
// stops working (the DOM quiet for 300ms, no short timer or data request
// pending, no loading indicator on screen; at most 5s, and at most 2s for DOM
// churn alone), nearby images (all images when whole is true) and finite
// entrance animations, each capped at 3s. Infinite animations never finish,
// so they are skipped. It reports what was still unsettled when the caps ran out.
const settleJS = `async (whole) => {
  const frames = () => new Promise(r => requestAnimationFrame(() => requestAnimationFrame(r)));
  // The page's own setTimeout is wrapped by changeTrackerJS; don't count our waits as its work.
  const after = (ms, v) => new Promise(r => (window.__linssiSetTimeout ?? setTimeout)(() => r(v), ms));
  if (document.fonts) await Promise.race([document.fonts.ready, after(3000)]).catch(() => {});
  // Same-origin frames run changeTrackerJS too; sum their state with ours.
  const wins = () => { const ws = [window]; for (let i = 0; i < window.frames.length; i++) ws.push(window.frames[i]); return ws; };
  const sum = f => wins().reduce((n, w) => { try { return n + (f(w) || 0); } catch { return n; } }, 0);
  const latest = key => wins().reduce((t, w) => { try { return Math.max(t, w[key] ?? 0); } catch { return t; } }, 0);
  const loaderSel = '[aria-busy="true"], [role="progressbar"], [class*="spinner" i], [class*="skeleton" i], [class*="shimmer" i], [class*="loader" i]';
  const shown = el => {
    const r = el.getBoundingClientRect();
    return r.width > 0 && r.height > 0 && r.top < innerHeight && r.bottom > 0 &&
      (!el.checkVisibility || el.checkVisibility({ opacityProperty: true, visibilityProperty: true }));
  };
  // DOM churn alone gets 2s, counted from the last moment other work was pending.
  let quiet = false, fetches = 0, loaders = 0;
  for (let t0 = performance.now(), busyAt = t0; ; await after(50)) {
    const now = performance.now();
    const still = now - latest('__linssiLastChange') >= 300;
    quiet = now - latest('__linssiLastContent') >= 300;
    const timers = sum(w => w.__linssiTimers?.size);
    fetches = sum(w => w.__linssiFetches?.());
    loaders = [...document.querySelectorAll(loaderSel)].filter(shown).length;
    if (timers || fetches || loaders) busyAt = now;
    else if (still || now - busyAt >= 2000) break;
    if (now - t0 >= 5000) break;
  }
  // Images that aren't rendered or are hidden (display: none, collapsed) may never load; don't wait on them.
  const near = img => {
    const b = img.getBoundingClientRect();
    if (!(b.width > 0 && b.height > 0)) return false;
    if (img.checkVisibility && !img.checkVisibility({ visibilityProperty: true })) return false; // e.g. in a collapsed table
    return whole || (b.bottom > -innerHeight / 2 && b.top < innerHeight * 1.5);
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
  return { images, animations, quiet, fetches, loaders };
}`

// changeTrackerJS runs before any page script. It records the time of the
// latest DOM change and of the latest content change (animation frames aside), the short timers set while the page starts (up to 2.5s,
// in its first 4s: the "show content in a moment" kind) and the fetch and XHR
// requests in flight, so settle can tell a finished page from a busy one.
const changeTrackerJS = `(() => {
  window.__linssiLastChange = performance.now();
  // JS animations rewrite inline styles and SVG attributes every frame. Settle
  // waits for that too (a JS fade-in), but only content changes mean the page
  // was still changing when captured.
  window.__linssiLastContent = performance.now();
  const motion = r => r.type === 'attributes' && (r.attributeName === 'style' || r.target instanceof SVGElement);
  new MutationObserver(rs => {
    window.__linssiLastChange = performance.now();
    if (!rs.every(motion)) window.__linssiLastContent = window.__linssiLastChange;
  }).observe(document, { subtree: true, childList: true, attributes: true, characterData: true });
  const timers = new Set(), setT = window.setTimeout, clearT = window.clearTimeout;
  window.__linssiTimers = timers;
  window.__linssiSetTimeout = setT.bind(window);
  window.setTimeout = function (fn, ms, ...args) {
    const d = Number(ms) || 0;
    if (typeof fn !== 'function' || d > 2500 || performance.now() > 4000) return setT.call(window, fn, ms, ...args);
    const id = setT.call(window, function (...a) { timers.delete(id); return fn.apply(this, a); }, ms, ...args);
    timers.add(id);
    return id;
  };
  window.clearTimeout = function (id) { timers.delete(id); return clearT.call(window, id); };
  let fetches = 0;
  window.__linssiFetches = () => fetches;
  const done = () => { fetches--; };
  if (window.fetch) {
    const f = window.fetch;
    window.fetch = function (...args) { fetches++; const p = f.apply(this, args); p.then(done, done); return p; };
  }
  const send = XMLHttpRequest.prototype.send;
  XMLHttpRequest.prototype.send = function (...args) {
    fetches++;
    this.addEventListener('loadend', done, { once: true });
    return send.apply(this, args);
  };
})()`

// scrollJS walks down the page one screen at a time so lazy content loads,
// for at most 5s, then returns to the top. It reports whether the page was
// still growing (an endless feed).
const scrollJS = `(async () => {
  const height = () => Math.max(document.body?.scrollHeight ?? 0, document.documentElement.scrollHeight);
  let y = 0;
  for (const end = performance.now() + 5000; y < height() && performance.now() < end; y += Math.max(innerHeight, 200)) {
    scrollTo(0, y);
    await new Promise(r => setTimeout(r, 50));
  }
  const endless = y < height();
  scrollTo(0, 0);
  await new Promise(r => setTimeout(r, 150));
  await new Promise(r => requestAnimationFrame(() => requestAnimationFrame(r)));
  return endless;
})()`

// probeJS reads the page state the report needs.
const probeJS = `(() => {
  const nav = performance.getEntriesByType('navigation')[0];
  const body = document.body;
  const text = (body?.innerText ?? '').trim().length;
  const media = document.querySelector('img, svg, canvas, video, picture, iframe');
  // A sign-in wall served in place, with no redirect: a visible password field,
  // a "Sign in with Google" / "Continue with Apple" style button (by its text or
  // accessible name, since SDK buttons are often SVG), or the Apple and Google
  // sign-in SDK buttons themselves.
  const onScreen = el => { const r = el.getBoundingClientRect(); return r.width > 0 && r.height > 0 && r.top < innerHeight && r.bottom > 0; };
  const provider = /\b(sign|log)\s?(in|up)\s+with\b|\bcontinue\s+with\s+(google|apple|github|microsoft|email|sso)\b/i;
  const named = el => [el.innerText, el.getAttribute('aria-label'), el.getAttribute('title')].some(s => s && provider.test(s));
  const sdk = '#appleid-signin, .g_id_signin, iframe[src*="accounts.google.com/gsi"]';
  const authWall = [...document.querySelectorAll('input[type=password], ' + sdk)].some(onScreen) ||
    [...document.querySelectorAll('button, a, [role=button]')].some(el => named(el) && onScreen(el));
  // An error page served with a success status: the title or the main heading says so.
  const h1 = (document.querySelector('h1')?.innerText ?? '').trim();
  const errorPage = /\b404\b|\bnot found\b|page (doesn.t|does not) exist|internal server error/i.test(document.title) ||
    (h1.length < 60 && /^(404|not found|page not found|500|server error)\b/i.test(h1));
  // A bot check (Cloudflare, Akamai, PerimeterX, DDoS-Guard) in place of the page.
  const challenge = /^(just a moment|attention required|checking your browser|access denied|ddos-guard|verify you are human)/i.test(document.title.trim()) ||
    !!document.querySelector('#challenge-form, #challenge-running, #cf-challenge-running, .cf-browser-verification, #px-captcha');
  // A modal or cookie wall over the page: a fixed layer under the screen's
  // center covering half the screen or more, that is a dialog or talks about cookies.
  let overlay = false;
  for (let el = document.elementFromPoint(innerWidth / 2, innerHeight / 2); el && el !== body && el !== document.documentElement; el = el.parentElement) {
    if (getComputedStyle(el).position !== 'fixed') continue;
    const r = el.getBoundingClientRect();
    if (r.width * r.height < innerWidth * innerHeight / 2) continue;
    overlay = el.matches('dialog, [role=dialog], [role=alertdialog], [aria-modal=true]') ||
      !!el.querySelector('dialog, [role=dialog], [role=alertdialog], [aria-modal=true]') ||
      /cookie|consent|privacy|gdpr/i.test(el.innerText ?? '');
    if (overlay) break;
  }
  return {
    url: location.href,
    status: nav && nav.responseStatus ? nav.responseStatus : null,
    title: document.title,
    blank: !body || (text === 0 && !media),
    authWall, errorPage, challenge, overlay,
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

// expectTextJS polls until the page's visible text contains text, ignoring
// case and runs of whitespace, or fails when the budget runs out.
func expectTextJS(text string, budgetMS int64) string {
	return fmt.Sprintf(`(async () => {
  const norm = s => s.replace(/\s+/g, ' ').trim().toLowerCase();
  const want = norm(%s), deadline = Date.now() + %d;
  while (!norm(document.body?.innerText ?? '').includes(want)) {
    if (Date.now() > deadline) throw new Error('expected text never appeared: ' + %s);
    await new Promise(r => setTimeout(r, 100));
  }
})()`, quote(text), budgetMS, quote(quote(text)))
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
