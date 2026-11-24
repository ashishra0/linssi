package linssi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/chromedp/cdproto/browser"
	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/page"
	cdpruntime "github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/remote"
)

const iphoneUA = "Mozilla/5.0 (iPhone; CPU iPhone OS 18_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.0 Mobile/15E148 Safari/604.1"

// maxTiles caps how many images one tall full-page capture returns.
const maxTiles = 8

// Viewport is the emulated screen, in CSS pixels.
type Viewport struct {
	Width, Height int
	Scale         float64
	Mobile        bool
}

// Options describe one capture.
type Options struct {
	Viewport Viewport
	Mode     string // "viewport", "full_page" or "element"
	Selector string
	Padding  int
	Dark     bool
	Wait     time.Duration
	WaitFor  string
	// ExpectText fails the capture unless this text (case-insensitive) shows
	// on the page within the timeout.
	ExpectText string
	Timeout    time.Duration
	Format     string // "png", "jpeg" or "webp"
	MaxPixels  int64  // per image; 0 means no budget
	Freeze     bool
	Hide       []string
	Mask       []string
	Headers    map[string]string
	Storage    *StorageState
}

// PageInfo is what the page looked like when the shutter fired.
type PageInfo struct {
	FinalURL   string   `json:"final_url"`
	HTTPStatus *int     `json:"http_status,omitempty"`
	Title      string   `json:"title"`
	Warnings   []string `json:"warnings"`
	ElapsedMS  int64    `json:"elapsed_ms"`
}

// Capture is one finished shot: a single image, or top-to-bottom tiles.
type Capture struct {
	CSSWidth, CSSHeight int
	Scale               float64
	Images              [][]byte
	Page                PageInfo
}

// Bytes is the total encoded size of all images.
func (c *Capture) Bytes() int {
	n := 0
	for _, img := range c.Images {
		n += len(img)
	}
	return n
}

// Camera owns one browser connection; captures run in their own tabs.
type Camera struct {
	browserCtx context.Context
	cancel     func()
	userAgent  string
	attached   bool
	profileDir string
}

// Launch starts a headless Chrome-family browser with a throwaway profile.
func Launch(chromePath string) (*Camera, error) {
	dir, err := os.MkdirTemp("", "linssi-chrome-")
	if err != nil {
		return nil, err
	}
	opts := append(chromedp.DefaultExecAllocatorOptions[:], chromedp.UserDataDir(dir))
	if chromePath == "" {
		chromePath = FindChrome()
	}
	if chromePath != "" {
		opts = append(opts, chromedp.ExecPath(chromePath))
	}
	allocCtx, cancelAlloc := chromedp.NewExecAllocator(context.Background(), opts...)
	browserCtx, cancelBrowser := chromedp.NewContext(allocCtx)
	v, err := chromedp.CallBrowser(browserCtx, browser.GetVersion, cdp.Empty{})
	if err != nil {
		cancelBrowser()
		cancelAlloc()
		os.RemoveAll(dir)
		return nil, fmt.Errorf("failed to launch Chrome (install Chrome, Chromium, Edge or Brave, or pass --chrome): %w", err)
	}
	return &Camera{
		browserCtx: browserCtx,
		cancel: func() {
			ctx, done := context.WithTimeout(browserCtx, 5*time.Second)
			defer done()
			chromedp.Cancel(ctx)
			cancelBrowser()
			cancelAlloc()
		},
		// Sites serve degraded pages to "HeadlessChrome".
		userAgent:  strings.ReplaceAll(v.UserAgent, "HeadlessChrome", "Chrome"),
		profileDir: dir,
	}, nil
}

// Attach connects to a Chrome the user already runs with
// --remote-debugging-port, so captures reuse its cookies and logins. Closing
// the camera never closes that browser.
func Attach(target string) (*Camera, error) {
	addr := connectURL(target)
	allocCtx, cancelAlloc := remote.NewAllocator(context.Background(), addr)
	browserCtx, cancelBrowser := chromedp.NewContext(allocCtx)
	if _, err := chromedp.CallBrowser(browserCtx, browser.GetVersion, cdp.Empty{}); err != nil {
		cancelBrowser()
		cancelAlloc()
		return nil, fmt.Errorf("can't reach a Chrome at %s; run 'linssi browser' to open one, or start Chrome with --remote-debugging-port and its own --user-data-dir", addr)
	}
	return &Camera{
		browserCtx: browserCtx,
		cancel: func() {
			cancelBrowser()
			cancelAlloc()
		},
		attached: true,
	}, nil
}

// Alive reports whether the browser connection still works.
func (c *Camera) Alive() bool { return c.browserCtx.Err() == nil }

// Close shuts a launched browser down and deletes its profile. An attached
// browser keeps running.
func (c *Camera) Close() {
	c.cancel()
	if c.profileDir != "" {
		os.RemoveAll(c.profileDir)
	}
}

// Capture loads url in a fresh tab and takes the shot described by o.
func (c *Camera) Capture(ctx context.Context, rawURL string, o Options) (*Capture, error) {
	started := time.Now()
	tab, closeTab := chromedp.NewContext(c.browserCtx)
	defer closeTab()
	// The first call opens the tab; only later calls may carry a deadline.
	if err := chromedp.Do(tab); err != nil {
		return nil, fmt.Errorf("opening a tab: %w", err)
	}
	ctx, cancel := context.WithTimeout(mergeCancel(tab, ctx), o.Timeout)
	defer cancel()

	var (
		mu     sync.Mutex
		errs   []string
		listen sync.WaitGroup
	)
	note := func(s string) {
		mu.Lock()
		defer mu.Unlock()
		if len(errs) < 5 {
			s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
			if len(s) > 200 {
				s = s[:200]
			}
			errs = append(errs, s)
		}
	}
	listenCtx, stopListening := context.WithCancel(ctx)
	exceptions := chromedp.Events(listenCtx, cdpruntime.ExceptionThrown)
	consoles := chromedp.Events(listenCtx, cdpruntime.ConsoleAPICalled)
	listen.Go(func() {
		for ev, err := range exceptions {
			if err != nil {
				return
			}
			if d := ev.ExceptionDetails; d != nil {
				if d.Exception != nil && d.Exception.Description != "" {
					note(d.Exception.Description)
				} else {
					note(d.Text)
				}
			}
		}
	})
	listen.Go(func() {
		for ev, err := range consoles {
			if err != nil {
				return
			}
			if ev.Type != cdpruntime.ConsoleAPICalledTypeError {
				continue
			}
			parts := make([]string, 0, len(ev.Args))
			for _, a := range ev.Args {
				var s string
				if json.Unmarshal(a.Value, &s) == nil {
					parts = append(parts, s)
				} else if a.Description != "" {
					parts = append(parts, a.Description)
				}
			}
			note(strings.Join(parts, " "))
		}
	})

	shot, err := c.shoot(ctx, rawURL, o, started)
	stopListening()
	listen.Wait()
	if errors.Is(err, context.DeadlineExceeded) {
		return nil, fmt.Errorf("timed out after %s", o.Timeout)
	}
	if err != nil {
		return nil, err
	}
	if len(errs) > 0 {
		shot.Page.Warnings = append(shot.Page.Warnings,
			fmt.Sprintf("%d console error(s): %s", len(errs), strings.Join(errs, " | ")))
	}
	shot.Page.ElapsedMS = time.Since(started).Milliseconds()
	return shot, nil
}

func (c *Camera) shoot(ctx context.Context, rawURL string, o Options, started time.Time) (*Capture, error) {
	v := o.Viewport
	if _, err := chromedp.Call(ctx, emulation.SetDeviceMetricsOverride, emulation.SetDeviceMetricsOverrideParams{
		Width: int64(v.Width), Height: int64(v.Height), DeviceScaleFactor: v.Scale, Mobile: v.Mobile,
	}); err != nil {
		return nil, err
	}
	ua := c.userAgent
	if v.Mobile {
		ua = iphoneUA
	}
	if ua != "" {
		if _, err := chromedp.Call(ctx, emulation.SetUserAgentOverride, emulation.SetUserAgentOverrideParams{UserAgent: ua}); err != nil {
			return nil, err
		}
	}
	// Pin the color scheme so captures don't follow the host OS appearance.
	scheme := "light"
	if o.Dark {
		scheme = "dark"
	}
	if _, err := chromedp.Call(ctx, emulation.SetEmulatedMedia, emulation.SetEmulatedMediaParams{
		Features: []*emulation.MediaFeature{{Name: "prefers-color-scheme", Value: scheme}},
	}); err != nil {
		return nil, err
	}
	if _, err := chromedp.Call(ctx, network.Enable, network.EnableParams{}); err != nil {
		return nil, err
	}
	if len(o.Headers) > 0 {
		headers := network.Headers{}
		for k, val := range o.Headers {
			headers[k] = val
		}
		if _, err := chromedp.Call(ctx, network.SetExtraHTTPHeaders, network.SetExtraHTTPHeadersParams{Headers: headers}); err != nil {
			return nil, err
		}
	}
	if s := o.Storage; s != nil {
		if len(s.Cookies) > 0 {
			if _, err := chromedp.Call(ctx, network.SetCookies, network.SetCookiesParams{Cookies: s.Cookies}); err != nil {
				return nil, fmt.Errorf("setting cookies: %w", err)
			}
		}
		if s.LocalStorageJS != "" {
			if _, err := chromedp.Call(ctx, page.AddScriptToEvaluateOnNewDocument, page.AddScriptToEvaluateOnNewDocumentParams{Source: s.LocalStorageJS}); err != nil {
				return nil, err
			}
		}
	}

	settled, warnings, err := c.load(ctx, rawURL, o, started)
	if err != nil {
		return nil, err
	}
	if warnings == nil {
		warnings = []string{} // JSON reports always carry a list
	}
	if o.Freeze || len(o.Hide) > 0 || len(o.Mask) > 0 {
		if _, err := eval[any](ctx, stabilizeJS(o.Freeze, o.Hide, o.Mask)); err != nil {
			return nil, err
		}
	}

	p, err := eval[probeResult](ctx, probeJS)
	if err != nil {
		return nil, fmt.Errorf("reading page state: %w", err)
	}
	if p.Status != nil && *p.Status >= 400 {
		warnings = append(warnings, fmt.Sprintf("page returned HTTP %d", *p.Status))
	}
	if looksLikeLogin(rawURL, p.URL) {
		warnings = append(warnings, fmt.Sprintf("redirected to what looks like a login page (%s); pass storage_state or headers, or connect to a logged-in browser", p.URL))
	} else if p.AuthWall && !isLoginURL(rawURL) {
		warnings = append(warnings, "page shows a sign-in or sign-up screen, not signed-in content; pass storage_state or headers, or connect to a logged-in browser")
	}
	if p.Challenge {
		warnings = append(warnings, fmt.Sprintf("page is a bot check (%q), not the site; the site blocked the headless browser", p.Title))
	} else if p.ErrorPage && (p.Status == nil || *p.Status < 400) {
		warnings = append(warnings, fmt.Sprintf("page looks like an error page (%q) though it returned a success status", p.Title))
	}
	if p.Overlay {
		warnings = append(warnings, "a dialog or cookie wall covers the page; dismiss it first, or pass hide with its selector")
	}
	if p.Blank {
		warnings = append(warnings, "page looks blank: no text or media rendered")
	}
	if settled.Images > 0 {
		warnings = append(warnings, fmt.Sprintf("%d image(s) still loading after 3s", settled.Images))
	}
	if settled.Animations && !o.Freeze {
		warnings = append(warnings, "animations still running after 3s; try freeze")
	}
	if !settled.Quiet && !o.Freeze {
		warnings = append(warnings, "page content was still changing when captured")
	}
	if settled.Fetches > 0 {
		warnings = append(warnings, fmt.Sprintf("%d data request(s) still loading after 5s; content may be missing", settled.Fetches))
	}
	if settled.Loaders > 0 {
		warnings = append(warnings, "a loading indicator (spinner, skeleton or progress bar) is still on screen")
	}
	if settled.Endless {
		warnings = append(warnings, "page kept growing while scrolling (an endless feed?); captured what loaded in 5s")
	}

	// A phone shows a page without a viewport meta zoomed out to its layout
	// width (usually 980px); capture that whole width, scaled to the screen.
	zoom, layoutW, layoutH := 1.0, float64(v.Width), float64(v.Height)
	if v.Mobile && p.InnerWidth > float64(v.Width)+1 {
		zoom, layoutW, layoutH = float64(v.Width)/p.InnerWidth, p.InnerWidth, p.InnerHeight
	}
	var region rect
	tileable := false
	switch o.Mode {
	case "full_page":
		region = rect{0, 0, layoutW, math.Max(p.DocHeight, layoutH)}
		tileable = true
	case "element":
		b, err := eval[bounds](ctx, boundsJS(o.Selector))
		if err != nil {
			return nil, err
		}
		if region, err = clip(b, o.Padding); err != nil {
			return nil, fmt.Errorf("cannot capture %s: %w", o.Selector, err)
		}
	default:
		region = rect{p.ScrollX, p.ScrollY, layoutW, layoutH}
	}

	plan := planTiles(int(region.W), int(region.H), v.Scale*zoom, o.MaxPixels, tileable)
	// Only warn when text really gets small; a few percent below 1x is fine.
	if plan.Scale < 0.75*zoom && plan.Scale < v.Scale*zoom {
		warnings = append(warnings, fmt.Sprintf("downscaled to %.2fx to fit max_pixels; small text may be hard to read", plan.Scale))
	}
	if plan.Truncated {
		warnings = append(warnings, fmt.Sprintf("page is %d css px tall; captured the top %d css px in %d tiles",
			int(region.H), plan.TileHeight*plan.Tiles, plan.Tiles))
		region.H = float64(plan.TileHeight * plan.Tiles)
	}

	images := make([][]byte, 0, plan.Tiles)
	for i := range plan.Tiles {
		top := float64(i * plan.TileHeight)
		if top >= region.H {
			break
		}
		params := page.CaptureScreenshotParams{
			Format: page.CaptureScreenshotFormat(o.Format),
			Clip: &page.Viewport{
				X: region.X, Y: region.Y + top, Width: region.W,
				Height: math.Min(region.H-top, float64(plan.TileHeight)),
				Scale:  plan.Scale / v.Scale,
			},
			CaptureBeyondViewport: ptr(true),
		}
		if o.Format != "png" {
			params.Quality = ptr(int64(90))
		}
		res, err := chromedp.Call(ctx, page.CaptureScreenshot, params)
		if err != nil {
			return nil, fmt.Errorf("screenshot: %w", err)
		}
		images = append(images, res.Data)
	}

	if !p.Blank && o.Mode != "element" && noInk(images) {
		warnings = append(warnings, "image is one flat color: nothing visible rendered (text the same color as the background, or content not painted yet)")
	}

	return &Capture{
		CSSWidth: int(region.W), CSSHeight: int(region.H), Scale: plan.Scale, Images: images,
		Page: PageInfo{FinalURL: p.URL, HTTPStatus: p.Status, Title: p.Title, Warnings: warnings},
	}, nil
}

// load navigates and waits until the page is worth photographing: the
// document parsed, its load event (or loadWait of trying, so one hung tracker
// pixel can't block the shot), the network quiet, then the in-page settle
// checks. A page that navigates on its own meanwhile, such as an SPA bouncing
// to /login, is followed to its new document, up to three times.
func (c *Camera) load(ctx context.Context, rawURL string, o Options, started time.Time) (settleResult, []string, error) {
	const loadWait = 5 * time.Second
	if _, err := chromedp.Call(ctx, page.AddScriptToEvaluateOnNewDocument, page.AddScriptToEvaluateOnNewDocumentParams{Source: changeTrackerJS}); err != nil {
		return settleResult{}, nil, err
	}
	w := watchPage(ctx)
	res, err := chromedp.Call(ctx, page.Navigate, page.NavigateParams{URL: rawURL})
	switch {
	case err != nil:
		return settleResult{}, nil, err
	case res.ErrorText != "":
		return settleResult{}, nil, fmt.Errorf("page load error %s", res.ErrorText)
	case res.IsDownload:
		return settleResult{}, nil, errors.New("the URL is a file download, not a page")
	}
	var warnings []string
	stillLoading := false
	want := 1
	for attempt := 0; ; attempt++ {
		if !w.wait(ctx, 0, func() bool { return w.docs >= want }) {
			if ctx.Err() != nil {
				return settleResult{}, nil, ctx.Err()
			}
		}
		gen := w.snapshot()
		if !w.wait(ctx, loadWait, func() bool { return w.loads >= w.docs }) && ctx.Err() == nil && !stillLoading {
			stillLoading = true
			warnings = append(warnings, fmt.Sprintf("page still loading after %s (a request never finished); captured anyway", loadWait))
		}
		// A stuck request already cost loadWait; don't wait the full idle cap on it too.
		idleCap := 2 * time.Second
		if stillLoading {
			idleCap = time.Second
		}
		w.waitIdle(ctx, 300*time.Millisecond, idleCap)
		settled, err := c.settle(ctx, o, started)
		if err == nil {
			return settled, warnings, nil
		}
		if !isNavErr(err) {
			return settleResult{}, nil, err
		}
		if attempt == 2 {
			return settleResult{}, nil, errors.New("the page kept navigating during capture")
		}
		want = gen + 1
	}
}

// settle runs the in-page waits for the capture mode.
func (c *Camera) settle(ctx context.Context, o Options, started time.Time) (settleResult, error) {
	// Leave half a second so a selector wait fails with its own message, not a timeout.
	budget := func() int64 {
		left := o.Timeout - time.Since(started) - 500*time.Millisecond
		return max(left.Milliseconds(), 0)
	}
	settled, err := eval[settleResult](ctx, fmt.Sprintf("(%s)(false)", settleJS))
	if err != nil {
		return settled, err
	}
	endless := false
	if o.ExpectText != "" {
		if _, err := eval[any](ctx, expectTextJS(o.ExpectText, budget())); err != nil {
			return settled, err
		}
		// The text arrived; let what came with it finish too.
		if settled, err = eval[settleResult](ctx, fmt.Sprintf("(%s)(false)", settleJS)); err != nil {
			return settled, err
		}
	}
	if o.WaitFor != "" {
		if _, err := eval[any](ctx, waitForJS(o.WaitFor, budget(), false)); err != nil {
			return settled, err
		}
	}
	switch o.Mode {
	case "full_page":
		if endless, err = eval[bool](ctx, scrollJS); err != nil {
			return settled, err
		}
	case "element":
		if _, err := eval[any](ctx, waitForJS(o.Selector, budget(), true)); err != nil {
			return settled, err
		}
		// Scrolling into view can start lazy loads and entrance animations.
		if settled, err = eval[settleResult](ctx, fmt.Sprintf("(%s)(false)", settleJS)); err != nil {
			return settled, err
		}
	}
	if o.Wait > 0 {
		select {
		case <-time.After(o.Wait):
		case <-ctx.Done():
			return settled, ctx.Err()
		}
		if settled, err = eval[settleResult](ctx, fmt.Sprintf("(%s)(false)", settleJS)); err != nil {
			return settled, err
		}
	}
	if o.Mode == "full_page" {
		if settled, err = eval[settleResult](ctx, fmt.Sprintf("(%s)(true)", settleJS)); err != nil {
			return settled, err
		}
	}
	settled.Endless = endless
	return settled, nil
}

// isNavErr reports whether err means the page replaced its document mid-call.
func isNavErr(err error) bool {
	msg := err.Error()
	for _, s := range []string{"navigated or closed", "Execution context was destroyed", "Cannot find context with specified id", "Cannot find default execution context"} {
		if strings.Contains(msg, s) {
			return true
		}
	}
	return false
}

// pageWatch counts document and load events and tracks in-flight requests of
// one tab, so waits can be bounded instead of blocking on the load event.
type pageWatch struct {
	mu          sync.Mutex
	changed     chan struct{} // closed and replaced on every update
	docs, loads int
	inflight    map[network.RequestID]struct{}
	lastNet     time.Time
}

func watchPage(ctx context.Context) *pageWatch {
	w := &pageWatch{changed: make(chan struct{}), inflight: map[network.RequestID]struct{}{}, lastNet: time.Now()}
	// Subscribe before navigating so no event is missed.
	dcl := chromedp.Events(ctx, page.DomContentEventFired)
	load := chromedp.Events(ctx, page.LoadEventFired)
	sent := chromedp.Events(ctx, network.RequestWillBeSent)
	done := chromedp.Events(ctx, network.LoadingFinished)
	failed := chromedp.Events(ctx, network.LoadingFailed)
	go func() {
		for _, err := range dcl {
			if err != nil {
				return
			}
			w.update(func() { w.docs++ })
		}
	}()
	go func() {
		for _, err := range load {
			if err != nil {
				return
			}
			w.update(func() { w.loads++ })
		}
	}()
	go func() {
		for ev, err := range sent {
			if err != nil {
				return
			}
			w.update(func() { w.inflight[ev.RequestID] = struct{}{}; w.lastNet = time.Now() })
		}
	}()
	go func() {
		for ev, err := range done {
			if err != nil {
				return
			}
			w.update(func() { delete(w.inflight, ev.RequestID); w.lastNet = time.Now() })
		}
	}()
	go func() {
		for ev, err := range failed {
			if err != nil {
				return
			}
			w.update(func() { delete(w.inflight, ev.RequestID); w.lastNet = time.Now() })
		}
	}()
	return w
}

func (w *pageWatch) update(f func()) {
	w.mu.Lock()
	defer w.mu.Unlock()
	f()
	close(w.changed)
	w.changed = make(chan struct{})
}

func (w *pageWatch) snapshot() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.docs
}

// wait blocks until cond holds (checked under the lock), the timeout passes
// (0 means none), or ctx ends. It reports whether cond held.
func (w *pageWatch) wait(ctx context.Context, timeout time.Duration, cond func() bool) bool {
	var expired <-chan time.Time
	if timeout > 0 {
		t := time.NewTimer(timeout)
		defer t.Stop()
		expired = t.C
	}
	for {
		w.mu.Lock()
		ok, ch := cond(), w.changed
		w.mu.Unlock()
		if ok {
			return true
		}
		select {
		case <-ch:
		case <-expired:
			return false
		case <-ctx.Done():
			return false
		}
	}
}

// waitIdle waits until no request has been in flight for quiet, at most limit.
// Long polls and stuck requests just use up the limit.
func (w *pageWatch) waitIdle(ctx context.Context, quiet, limit time.Duration) {
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		w.mu.Lock()
		idle := len(w.inflight) == 0 && time.Since(w.lastNet) >= quiet
		w.mu.Unlock()
		if idle {
			return
		}
		select {
		case <-time.After(50 * time.Millisecond):
		case <-ctx.Done():
			return
		}
	}
}

// eval runs a JS expression in the page, awaiting promises, and decodes the result.
func eval[T any](ctx context.Context, js string) (T, error) {
	v, err := chromedp.Run(ctx, chromedp.Evaluate[T](js, chromedp.EvalAwaitPromise))
	var ex *chromedp.ExceptionError
	if errors.As(err, &ex) {
		msg := ex.Text
		if ex.Exception != nil && ex.Exception.Description != "" {
			msg = ex.Exception.Description
		}
		msg, _, _ = strings.Cut(msg, "\n")
		return v, errors.New(strings.TrimPrefix(msg, "Error: "))
	}
	return v, err
}

// mergeCancel returns base, canceled also when other is canceled, so a
// caller's context (an MCP request) can stop a capture.
func mergeCancel(base, other context.Context) context.Context {
	ctx, cancel := context.WithCancel(base)
	stop := context.AfterFunc(other, cancel)
	context.AfterFunc(ctx, func() { stop() })
	return ctx
}

func ptr[T any](v T) *T { return &v }

type settleResult struct {
	Images     int  `json:"images"`
	Animations bool `json:"animations"`
	Quiet      bool `json:"quiet"`
	Fetches    int  `json:"fetches"`
	Loaders    int  `json:"loaders"`
	Endless    bool `json:"-"`
}

type probeResult struct {
	URL         string  `json:"url"`
	Status      *int    `json:"status"`
	Title       string  `json:"title"`
	Blank       bool    `json:"blank"`
	AuthWall    bool    `json:"authWall"`
	ErrorPage   bool    `json:"errorPage"`
	Challenge   bool    `json:"challenge"`
	Overlay     bool    `json:"overlay"`
	ScrollX     float64 `json:"scrollX"`
	ScrollY     float64 `json:"scrollY"`
	DocHeight   float64 `json:"docHeight"`
	InnerWidth  float64 `json:"innerWidth"`
	InnerHeight float64 `json:"innerHeight"`
}

type bounds struct {
	X, Y, W, H, DocW, DocH float64
}

type rect struct{ X, Y, W, H float64 }

// clip pads an element's box, rounds it outward to whole pixels, and keeps it
// inside the document.
func clip(b bounds, padding int) (rect, error) {
	pad := float64(padding)
	left := math.Max(0, math.Floor(b.X-pad))
	top := math.Max(0, math.Floor(b.Y-pad))
	right := math.Min(b.DocW, math.Ceil(b.X+b.W+pad))
	bottom := math.Min(b.DocH, math.Ceil(b.Y+b.H+pad))
	if right <= left || bottom <= top {
		return rect{}, errors.New("element is outside the document")
	}
	return rect{left, top, right - left, bottom - top}, nil
}

type tilePlan struct {
	Scale      float64
	TileHeight int // CSS px per tile
	Tiles      int
	Truncated  bool
}

// maxDeviceRows is the tallest single capture Chrome renders correctly; past
// ~16,384 device px it wraps content around.
const maxDeviceRows = 16000

// planTiles fits a width×height CSS-px region into maxPixels output pixels per
// image and into Chrome's render limit. Viewport and element shots only scale
// down. A tall full page splits into even tiles at up to 1x instead, because
// legibility matters more than one image; past maxTiles it shrinks toward
// 0.5x, then truncates. With no budget, only Chrome's limit applies.
func planTiles(width, height int, scale float64, maxPixels int64, tileable bool) tilePlan {
	w, h, budget := float64(max(width, 1)), float64(max(height, 1)), float64(maxPixels)
	s := scale
	if maxPixels > 0 {
		// 0.995 leaves room for rounding device pixels up.
		if fit := math.Sqrt(budget/(w*h)) * 0.995; fit < s {
			if !tileable || fit >= math.Min(s, 1) {
				s = fit
			} else {
				s = math.Min(s, 1)
			}
		}
	}
	if !tileable {
		s = math.Min(s, maxDeviceRows/h)
		return tilePlan{Scale: s, TileHeight: int(h), Tiles: 1}
	}
	layout := func(s float64) (int, int) {
		rows := float64(maxDeviceRows)
		if maxPixels > 0 {
			rows = math.Min(rows, math.Floor(budget/math.Ceil(w*s)))
		}
		th := int(rows / s)
		if math.Ceil(float64(th)*s) > rows { // rounding up a fractional scale would go over
			th--
		}
		th = max(th, 1)
		return th, (int(h) + th - 1) / th
	}
	limit := 64
	if maxPixels > 0 {
		limit = maxTiles
	}
	th, n := layout(s)
	if n > limit && maxPixels > 0 {
		s = math.Min(s, math.Max(0.5, math.Sqrt(float64(limit)*budget/(w*h))*0.98))
		th, n = layout(s)
	}
	if n > limit {
		return tilePlan{Scale: s, TileHeight: th, Tiles: limit, Truncated: true}
	}
	// Split evenly so the last tile isn't a sliver.
	return tilePlan{Scale: s, TileHeight: (int(h) + n - 1) / n, Tiles: n}
}

// looksLikeLogin reports whether the page ended on a sign-in route the caller
// didn't ask for. It matches whole path segments, so /authors is not /auth.
func looksLikeLogin(requested, final string) bool {
	return isLoginURL(final) && !isLoginURL(requested)
}

var loginSegments = map[string]bool{
	"login": true, "log-in": true, "signin": true, "sign-in": true, "sign_in": true, "auth": true,
	"sso": true, "oauth": true, "oauth2": true, "session": true, "sessions": true, "authorize": true,
	"signup": true, "sign-up": true, "register": true,
}

// isLoginURL reports whether raw is a sign-in or sign-up route.
func isLoginURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	host := strings.ToLower(u.Hostname())
	if strings.HasPrefix(host, "login.") || strings.HasPrefix(host, "auth.") || strings.HasPrefix(host, "sso.") || host == "accounts.google.com" {
		return true
	}
	for _, seg := range strings.Split(strings.ToLower(u.Path), "/") {
		if loginSegments[seg] {
			return true
		}
	}
	return false
}

// connectURL turns "9222", "host:9222" or a full URL into a DevTools address.
func connectURL(target string) string {
	switch {
	case strings.Contains(target, "://"):
		return target
	case strings.Trim(target, "0123456789") == "":
		return "http://127.0.0.1:" + target
	default:
		return "http://" + target
	}
}

// NormalizeURL adds a scheme: http for localhost and loopback, https otherwise.
func NormalizeURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", errors.New("empty URL")
	}
	if strings.Contains(raw, "://") {
		u, err := url.Parse(raw)
		if err != nil {
			return "", fmt.Errorf("invalid URL %q: %w", raw, err)
		}
		return u.String(), nil
	}
	u, err := url.Parse("http://" + raw)
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("invalid URL %q", raw)
	}
	host := strings.ToLower(u.Hostname())
	local := host == "localhost" || strings.HasSuffix(host, ".localhost")
	if ip := net.ParseIP(host); ip != nil && (ip.IsLoopback() || ip.IsUnspecified()) {
		local = true
	}
	if !local {
		u.Scheme = "https"
	}
	if u.Path == "" {
		u.Path = "/"
	}
	return u.String(), nil
}

// ParseViewport reads "desktop", "iphone", "ipad" or "WxH". scale 0 keeps the preset's.
func ParseViewport(size string, scale float64) (Viewport, error) {
	var v Viewport
	switch size {
	case "", "desktop":
		v = Viewport{1440, 900, 2, false}
	case "iphone":
		v = Viewport{390, 844, 3, true}
	case "ipad":
		v = Viewport{1024, 1366, 2, false}
	default:
		dims, density, hasDensity := strings.Cut(strings.ToLower(size), "@")
		var w, h int
		var rest string
		if n, _ := fmt.Sscanf(dims, "%dx%d%s", &w, &h, &rest); n < 2 || rest != "" || w <= 0 || h <= 0 {
			return v, fmt.Errorf("invalid size %q: use WxH or WxH@Nx (like 1440x900@2x), or desktop, iphone, ipad", size)
		}
		v = Viewport{w, h, 2, false}
		if hasDensity {
			d, err := strconv.ParseFloat(strings.TrimSuffix(density, "x"), 64)
			if err != nil || d <= 0 || math.IsInf(d, 0) {
				return v, fmt.Errorf("invalid size %q: the part after @ must be like 2x", size)
			}
			v.Scale = d
		}
	}
	if scale != 0 {
		if math.IsNaN(scale) || math.IsInf(scale, 0) || scale < 0 {
			return v, fmt.Errorf("invalid scale %v: use a number greater than zero", scale)
		}
		v.Scale = scale
	}
	return v, nil
}

// StorageState is the cookies and localStorage of a logged-in session.
type StorageState struct {
	Cookies        []*network.CookieParam
	LocalStorageJS string
}

// LoadStorageState reads a Playwright storage-state file or a cookie-array export.
func LoadStorageState(path string) (*StorageState, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading storage state: %w", err)
	}
	s, err := ParseStorageState(data)
	if err != nil {
		return nil, fmt.Errorf("invalid storage state %s: %w", path, err)
	}
	return s, nil
}

// ParseStorageState accepts Playwright's {"cookies", "origins"} format or a
// bare cookie array as browser extensions export it.
func ParseStorageState(data []byte) (*StorageState, error) {
	var state struct {
		Cookies []map[string]any `json:"cookies"`
		Origins []struct {
			Origin       string `json:"origin"`
			LocalStorage []struct {
				Name  string `json:"name"`
				Value string `json:"value"`
			} `json:"localStorage"`
		} `json:"origins"`
	}
	trimmed := strings.TrimSpace(string(data))
	switch {
	case strings.HasPrefix(trimmed, "["):
		if err := json.Unmarshal(data, &state.Cookies); err != nil {
			return nil, err
		}
	case strings.HasPrefix(trimmed, "{"):
		if err := json.Unmarshal(data, &state); err != nil {
			return nil, err
		}
	default:
		return nil, errors.New("expected a storage-state object or a cookie array")
	}

	s := &StorageState{}
	for _, raw := range state.Cookies {
		// Extensions write expirationDate; Playwright marks session cookies with -1.
		if exp, ok := raw["expirationDate"]; ok {
			raw["expires"] = exp
			delete(raw, "expirationDate")
		}
		if exp, ok := raw["expires"].(float64); !ok || exp < 0 {
			delete(raw, "expires")
		}
		if same, ok := raw["sameSite"].(string); ok {
			switch strings.ToLower(same) {
			case "strict":
				raw["sameSite"] = "Strict"
			case "lax":
				raw["sameSite"] = "Lax"
			case "none", "no_restriction":
				raw["sameSite"] = "None"
			default:
				delete(raw, "sameSite")
			}
		}
		for _, k := range []string{"hostOnly", "session", "storeId", "id", "partitionKey"} {
			delete(raw, k)
		}
		buf, _ := json.Marshal(raw)
		var c network.CookieParam
		if err := json.Unmarshal(buf, &c); err != nil {
			return nil, fmt.Errorf("bad cookie: %w", err)
		}
		if c.Name == "" || (c.Domain == "" && c.URL == "") {
			return nil, errors.New("every cookie needs a name and a domain or url")
		}
		s.Cookies = append(s.Cookies, &c)
	}

	type item struct {
		Origin string            `json:"origin"`
		Items  map[string]string `json:"items"`
	}
	var origins []item
	for _, o := range state.Origins {
		it := item{Origin: o.Origin, Items: map[string]string{}}
		for _, kv := range o.LocalStorage {
			it.Items[kv.Name] = kv.Value
		}
		if o.Origin != "" && len(it.Items) > 0 {
			origins = append(origins, it)
		}
	}
	if len(origins) > 0 {
		buf, _ := json.Marshal(origins)
		s.LocalStorageJS = fmt.Sprintf(`(() => {
  for (const { origin, items } of %s) {
    if (location.origin !== origin) continue;
    for (const [k, v] of Object.entries(items)) { try { localStorage.setItem(k, v); } catch {} }
  }
})()`, buf)
	}
	return s, nil
}

// FindChrome prefers Linssi's own Chrome for Testing, then installed browser
// apps over PATH entries, which can be stale wrapper scripts. Captures run
// headless with a throwaway profile, so any of these is safe to use there.
func FindChrome() string {
	if p := DedicatedChrome(); p != "" {
		return p
	}
	var candidates []string
	switch runtime.GOOS {
	case "darwin":
		for _, app := range []string{"Google Chrome", "Chromium", "Google Chrome Canary", "Microsoft Edge", "Brave Browser"} {
			candidates = append(candidates, filepath.Join("/Applications", app+".app", "Contents", "MacOS", app))
		}
	case "windows":
		for _, base := range []string{os.Getenv("ProgramFiles"), os.Getenv("ProgramFiles(x86)"), os.Getenv("LocalAppData")} {
			candidates = append(candidates,
				filepath.Join(base, `Google\Chrome\Application\chrome.exe`),
				filepath.Join(base, `Microsoft\Edge\Application\msedge.exe`))
		}
	default:
		candidates = []string{"/usr/bin/google-chrome", "/usr/bin/google-chrome-stable", "/usr/bin/chromium", "/usr/bin/chromium-browser", "/snap/bin/chromium"}
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}
	for _, name := range []string{"google-chrome", "chromium", "chromium-browser", "chrome", "msedge"} {
		if p, err := exec.LookPath(name); err == nil {
			return p
		}
	}
	return "" // let chromedp try its own detection
}
