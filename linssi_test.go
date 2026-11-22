package linssi

import (
	"bytes"
	"context"
	"fmt"
	"image"
	_ "image/jpeg"
	"image/png"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestPlanTiles(t *testing.T) {
	// Each tile must fit the budget in real device pixels, and tiles must cover the page.
	check := func(name string, w, h int, p tilePlan, budget int64) {
		t.Helper()
		devW := int64(math.Ceil(float64(w) * p.Scale))
		devH := int64(math.Ceil(float64(p.TileHeight) * p.Scale))
		if budget > 0 && devW*devH > budget {
			t.Errorf("%s: tile %dx%d device px is over %d", name, devW, devH, budget)
		}
		if devH > maxDeviceRows {
			t.Errorf("%s: tile %d device rows is past Chrome's limit", name, devH)
		}
		if !p.Truncated && p.TileHeight*p.Tiles < h {
			t.Errorf("%s: %d tiles of %d don't cover %d", name, p.Tiles, p.TileHeight, h)
		}
		if p.Tiles > 1 && !p.Truncated && h-(p.Tiles-1)*p.TileHeight < p.TileHeight/2 {
			t.Errorf("%s: last tile is a sliver (%+v)", name, p)
		}
	}
	// No budget: Retina kept; past Chrome's limit, tiles instead of wrapped garbage (T-016).
	if p := planTiles(1440, 900, 2, 0, true); p != (tilePlan{2, 900, 1, false}) {
		t.Fatalf("no budget: %+v", p)
	}
	p := planTiles(1440, 20000, 2, 0, true)
	if p.Scale != 2 || p.Tiles != 3 {
		t.Fatalf("20k page: %+v", p)
	}
	check("20k page", 1440, 20000, p, 0)
	// Viewport over budget: one image, downscaled to fit.
	p = planTiles(1440, 900, 2, 1_150_000, false)
	if p.Tiles != 1 {
		t.Fatalf("viewport: %+v", p)
	}
	check("viewport", 1440, 900, p, 1_150_000)
	// Tall page: even tiles at 1x within budget.
	p = planTiles(1440, 4000, 2, 1_150_000, true)
	if p.Scale != 1 || p.Tiles < 2 || p.Truncated {
		t.Fatalf("tall: %+v", p)
	}
	check("tall", 1440, 4000, p, 1_150_000)
	// Very tall: shrink toward 0.5x to stay within maxTiles (T-027 budget check included).
	for _, h := range []int{10345, 12000, 15000} {
		p = planTiles(1440, h, 2, 1_150_000, true)
		if p.Scale >= 1 || p.Scale < 0.5 || p.Tiles > maxTiles || p.Truncated {
			t.Fatalf("very tall %d: %+v", h, p)
		}
		check(fmt.Sprint("very tall ", h), 1440, h, p, 1_150_000)
	}
	// Absurdly tall: truncate and say so.
	if p = planTiles(1440, 100_000, 2, 1_150_000, true); p.Scale != 0.5 || p.Tiles != maxTiles || !p.Truncated {
		t.Fatalf("absurd: %+v", p)
	}
	// Fits already.
	if p = planTiles(320, 240, 2, 1_150_000, true); p != (tilePlan{2, 240, 1, false}) {
		t.Fatalf("fits: %+v", p)
	}
}

func TestLooksLikeLogin(t *testing.T) {
	cases := []struct {
		req, final string
		want       bool
	}{
		{"http://localhost:3000/billing", "http://localhost:3000/login?next=/billing", true},
		{"https://app.dev/", "https://accounts.google.com/o/oauth2", true},
		{"http://localhost:3000/login", "http://localhost:3000/login", false},
		{"http://localhost:3000/a", "http://localhost:3000/a", false},
		{"http://localhost:3000/to-authors", "http://localhost:3000/authors", false},
		{"https://app.dev/", "https://login.app.dev/", true},
		{"https://app.dev/board", "https://app.dev/signup", true},
		{"https://app.dev/register", "https://app.dev/register", false},
	}
	for _, c := range cases {
		if got := looksLikeLogin(c.req, c.final); got != c.want {
			t.Errorf("looksLikeLogin(%q, %q) = %v", c.req, c.final, got)
		}
	}
}

func TestParseStorageState(t *testing.T) {
	pw, err := ParseStorageState([]byte(`{"cookies":[{"name":"sid","value":"abc","domain":"localhost","path":"/","expires":-1,"httpOnly":true,"secure":false,"sameSite":"Lax"}],
		"origins":[{"origin":"http://localhost:3000","localStorage":[{"name":"token","value":"t"}]}]}`))
	if err != nil || len(pw.Cookies) != 1 || pw.Cookies[0].Name != "sid" || pw.Cookies[0].Expires != 0 || !strings.Contains(pw.LocalStorageJS, "localhost:3000") {
		t.Fatalf("playwright: %+v %v", pw, err)
	}
	ext, err := ParseStorageState([]byte(`[{"name":"sid","value":"abc","domain":".app.dev","path":"/","expirationDate":1999999999,"hostOnly":false,"sameSite":"no_restriction","storeId":"0"}]`))
	if err != nil || len(ext.Cookies) != 1 || ext.Cookies[0].Expires == 0 || ext.Cookies[0].SameSite != "None" || ext.LocalStorageJS != "" {
		t.Fatalf("extension: %+v %v", ext, err)
	}
	for _, bad := range []string{`42`, `[{"value":"x"}]`} {
		if _, err := ParseStorageState([]byte(bad)); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
}

func TestURLsSizesAndNames(t *testing.T) {
	for in, want := range map[string]string{
		"localhost:3000":     "http://localhost:3000/",
		"app.localhost:4173": "http://app.localhost:4173/",
		"127.0.0.1:8080":     "http://127.0.0.1:8080/",
		"example.com":        "https://example.com/",
		"http://example.com": "http://example.com",
	} {
		if got, _ := NormalizeURL(in); got != want {
			t.Errorf("NormalizeURL(%q) = %q, want %q", in, got, want)
		}
	}
	for in, want := range map[string]string{"9222": "http://127.0.0.1:9222", "localhost:9333": "http://localhost:9333", "ws://h:1/devtools/browser/x": "ws://h:1/devtools/browser/x"} {
		if got := connectURL(in); got != want {
			t.Errorf("connectURL(%q) = %q", in, got)
		}
	}
	for _, bad := range []string{"0x900", "1440x0", "big"} {
		if _, err := ParseViewport(bad, 0); err == nil {
			t.Errorf("accepted size %q", bad)
		}
	}
	if v, _ := ParseViewport("iphone", 1); v != (Viewport{390, 844, 1, true}) {
		t.Errorf("iphone: %+v", v)
	}
	if v, err := ParseViewport("390x844@3x", 0); err != nil || v != (Viewport{390, 844, 3, false}) {
		t.Errorf("390x844@3x: %+v %v", v, err)
	}
	for _, bad := range []string{"390x844@", "390x844@zx", "390x844px"} {
		if _, err := ParseViewport(bad, 0); err == nil {
			t.Errorf("accepted size %q", bad)
		}
	}
	r, err := clip(bounds{X: 10.25, Y: 20.75, W: 100.5, H: 50.5, DocW: 200, DocH: 200}, 5)
	if err != nil || r != (rect{5, 15, 111, 62}) {
		t.Errorf("clip = %+v %v", r, err)
	}
	if r, _ := clip(bounds{X: 190.4, Y: 180.4, W: 20, H: 30, DocW: 200, DocH: 200}, 5); r != (rect{185, 175, 15, 25}) {
		t.Errorf("clip at edge = %+v", r)
	}
}

// --- browser tests ---

var (
	camOnce sync.Once
	testCam *Camera
	camErr  error
)

func camera(t *testing.T) *Camera {
	t.Helper()
	camOnce.Do(func() { testCam, camErr = Launch("") })
	if camErr != nil {
		t.Skipf("no browser: %v", camErr)
	}
	return testCam
}

func TestMain(m *testing.M) {
	code := m.Run()
	if testCam != nil {
		testCam.Close()
	}
	os.Exit(code)
}

// site serves pages that echo auth back, fail, redirect to a login, run tall,
// render nothing, and hold a precisely placed element.
func site() *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		c, _ := r.Cookie("sid")
		val := ""
		if c != nil {
			val = c.Value
		}
		fmt.Fprintf(w, `<title>%s|%s</title><p>hello</p><script>document.title += '|' + (localStorage.getItem('token') || '')</script>`, val, r.Header.Get("X-Test"))
	})
	mux.HandleFunc("/missing", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
		fmt.Fprint(w, "<h1>Not found</h1>")
	})
	mux.HandleFunc("/private", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/login", http.StatusFound) })
	mux.HandleFunc("/login", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<h1>Sign in</h1><input type="password"><button>Continue with Google</button>`)
	})
	// Sign-in walls served in place, with no redirect (as mymind does at /).
	mux.HandleFunc("/wall", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<h1>Your new mind is here</h1><button>Sign up with Google</button><a href="/login">Sign in here.</a>`)
	})
	// SDK buttons with no visible text: Apple's names itself only in aria-label.
	mux.HandleFunc("/sdkwall", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<h1>Welcome</h1><button id="appleid-signin"><div role="button" aria-label="Sign in with Apple" style="width:200px;height:40px"><svg width="200" height="40"></svg></div></button>`)
	})
	mux.HandleFunc("/pwform", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<h1>Welcome back</h1><form><input type="email"><input type="password"><button>Next</button></form>`)
	})
	// A plain "Sign in" nav link on a content page is not a wall.
	mux.HandleFunc("/marketing", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<nav><a href="/login">Sign in</a></nav><h1>Pricing</h1><p>Plans for teams.</p>`)
	})
	mux.HandleFunc("/tall", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<style>body{margin:0}.hero{height:100vh;background:#00f}.rest{height:3760px;background:#f00}</style><div class="hero"></div><div class="rest">tall</div>`)
	})
	mux.HandleFunc("/blank", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "<script>console.error('boom')</script>") })
	mux.HandleFunc("/box", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<style>body{margin:0}#box{position:absolute;left:40.5px;top:600.25px;width:100px;height:50px;background:#0a0;animation:in .4s ease-out}
@keyframes in{from{opacity:0}}</style><div style="height:2000px"></div><div id="box"></div><div id="zero" style="width:0;height:0"></div>`)
	})
	mux.HandleFunc("/scheme", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<script>document.title = matchMedia('(prefers-color-scheme: dark)').matches ? 'dark' : 'light'</script>`)
	})
	mux.HandleFunc("/hang", func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() })
	mux.HandleFunc("/tracker", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<title>Tracker</title><h1>Content</h1><img src="/hang" width=1 height=1>`)
	})
	// An SPA that asks its API who the user is after load, then bounces to /login.
	mux.HandleFunc("/spa", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<title>App</title><h1>Loading app</h1><script>addEventListener('load', () =>
			fetch('/api/me').then(r => { if (r.status === 401) location.replace('/login'); }))</script>`)
	})
	mux.HandleFunc("/api/me", func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(400 * time.Millisecond)
		w.WriteHeader(http.StatusUnauthorized)
	})
	mux.HandleFunc("/navaway", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<style>div{animation:s 10s forwards}@keyframes s{from{opacity:0}}</style><div>leaving</div><script>setTimeout(() => location.href = '/', 1500)</script>`)
	})
	mux.HandleFunc("/masks", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<style>body{margin:0}div{height:50px;background:#fff;color:#000}</style><div class="ts">12:34</div><div class="date">2026-11-18</div>`)
	})
	mux.HandleFunc("/nometa", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<style>body{margin:0}div{width:980px;height:100px;background:linear-gradient(90deg,#f00 0 50%,#00f 50% 100%)}</style><div></div>`)
	})
	return httptest.NewServer(mux)
}

func pngSize(t *testing.T, b []byte) (int, int) {
	t.Helper()
	cfg, err := png.DecodeConfig(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("not a png: %v", err)
	}
	return cfg.Width, cfg.Height
}

func opts(mode string) Options {
	return Options{Viewport: Viewport{320, 240, 1, false}, Mode: mode, Timeout: 10 * time.Second, Format: "png"}
}

func TestBrowserAuthReadinessAndTiling(t *testing.T) {
	cam := camera(t)
	srv := site()
	defer srv.Close()
	ctx := context.Background()

	state, err := ParseStorageState(fmt.Appendf(nil, `{"cookies":[{"name":"sid","value":"abc","domain":"127.0.0.1","path":"/","expires":-1}],
		"origins":[{"origin":%q,"localStorage":[{"name":"token","value":"tok"}]}]}`, srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	o := opts("viewport")
	o.Headers, o.Storage = map[string]string{"X-Test": "yes"}, state
	shot, err := cam.Capture(ctx, srv.URL+"/", o)
	if err != nil {
		t.Fatal(err)
	}
	if shot.Page.Title != "abc|yes|tok" || shot.Page.HTTPStatus == nil || *shot.Page.HTTPStatus != 200 || len(shot.Page.Warnings) != 0 {
		t.Fatalf("auth: %+v", shot.Page)
	}
	if w, h := pngSize(t, shot.Images[0]); w != 320 || h != 240 {
		t.Fatalf("viewport size %dx%d", w, h)
	}

	expect := func(path, want string) {
		t.Helper()
		shot, err := cam.Capture(ctx, srv.URL+path, opts("viewport"))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(strings.Join(shot.Page.Warnings, "\n"), want) {
			t.Fatalf("%s: want warning %q, got %q", path, want, shot.Page.Warnings)
		}
	}
	expect("/missing", "HTTP 404")
	expect("/private", "login page")
	expect("/wall", "sign-in or sign-up screen")
	expect("/pwform", "sign-in or sign-up screen")
	expect("/sdkwall", "sign-in or sign-up screen")
	for _, path := range []string{"/login", "/marketing"} { // asked for the login page itself; a nav link only
		shot, err := cam.Capture(ctx, srv.URL+path, opts("viewport"))
		if err != nil {
			t.Fatal(err)
		}
		if w := strings.Join(shot.Page.Warnings, "\n"); strings.Contains(w, "sign-in") || strings.Contains(w, "login page") {
			t.Fatalf("%s: unexpected auth warning %q", path, w)
		}
	}
	expect("/blank", "blank")
	expect("/blank", "boom")

	// Full page keeps the 100vh hero at viewport height and tiles within budget.
	o = opts("full_page")
	o.MaxPixels, o.Freeze = 320*1000, true
	shot, err = cam.Capture(ctx, srv.URL+"/tall", o)
	if err != nil {
		t.Fatal(err)
	}
	if len(shot.Images) != 4 || shot.CSSHeight != 4000 {
		t.Fatalf("tiles: %d, height %d", len(shot.Images), shot.CSSHeight)
	}
	for _, img := range shot.Images {
		if w, h := pngSize(t, img); w*h > 320*1000 {
			t.Fatalf("tile %dx%d over budget", w, h)
		}
	}
	first, _ := png.Decode(bytes.NewReader(shot.Images[0]))
	blueAt := func(img image.Image, y int) bool { r, _, b, _ := img.At(10, y).RGBA(); return b > r }
	if !blueAt(first, 230) || blueAt(first, 250) {
		t.Fatal("100vh hero is not viewport height in a full-page capture")
	}

	if _, err := cam.Capture(ctx, srv.URL+"/", Options{Mask: []string{"["}, Viewport: o.Viewport, Mode: "viewport", Timeout: 5 * time.Second, Format: "png"}); err == nil || !strings.Contains(err.Error(), "invalid selector: [") {
		t.Fatalf("bad mask selector: %v", err)
	}
}

func TestBrowserLoadingRegressions(t *testing.T) {
	cam := camera(t)
	srv := site()
	defer func() { srv.CloseClientConnections(); srv.Close() }()
	ctx := context.Background()
	warns := func(c *Capture) string { return strings.Join(c.Page.Warnings, "\n") }

	// T-017: captures are light unless --dark, whatever the host OS uses.
	for dark, want := range map[bool]string{false: "light", true: "dark"} {
		o := opts("viewport")
		o.Dark = dark
		shot, err := cam.Capture(ctx, srv.URL+"/scheme", o)
		if err != nil || shot.Page.Title != want {
			t.Fatalf("dark=%v: title %q, %v", dark, shot.Page.Title, err)
		}
	}
	// T-018: a request that never finishes doesn't cost the whole capture.
	o := opts("viewport")
	o.Timeout = 20 * time.Second
	start := time.Now()
	shot, err := cam.Capture(ctx, srv.URL+"/tracker", o)
	if err != nil || !strings.Contains(warns(shot), "still loading") || time.Since(start) > 15*time.Second {
		t.Fatalf("hung tracker: %v after %s, warnings %q", err, time.Since(start), warns(shot))
	}
	// T-019: a client-side redirect shortly after load is followed and flagged.
	if shot, err = cam.Capture(ctx, srv.URL+"/spa", opts("viewport")); err != nil || !strings.HasSuffix(shot.Page.FinalURL, "/login") || !strings.Contains(warns(shot), "login page") {
		t.Fatalf("spa redirect: %v, %+v", err, shot.Page)
	}
	// T-020: a page that navigates while settling is followed, not an error.
	if shot, err = cam.Capture(ctx, srv.URL+"/navaway", opts("viewport")); err != nil || shot.Page.FinalURL != srv.URL+"/" {
		t.Fatalf("navaway: %v", err)
	}
	// T-022: a comma list in one --mask covers every match in gray.
	o = opts("viewport")
	o.Mask = []string{".ts, .date"}
	shot, err = cam.Capture(ctx, srv.URL+"/masks", o)
	if err != nil {
		t.Fatal(err)
	}
	img, _ := png.Decode(bytes.NewReader(shot.Images[0]))
	for _, y := range []int{25, 75} {
		if r, g, b, _ := img.At(100, y).RGBA(); r>>8 != 0x88 || g>>8 != 0x88 || b>>8 != 0x88 {
			t.Fatalf("row %d not masked gray", y)
		}
	}
	// T-021: a phone shows a page without a viewport meta at its whole layout width.
	o = opts("viewport")
	o.Viewport = Viewport{390, 844, 1, true}
	if shot, err = cam.Capture(ctx, srv.URL+"/nometa", o); err != nil {
		t.Fatal(err)
	}
	img, _ = png.Decode(bytes.NewReader(shot.Images[0]))
	if w := img.Bounds().Dx(); w != 390 {
		t.Fatalf("phone width %d", w)
	}
	if r, _, b, _ := img.At(380, 10).RGBA(); b <= r {
		t.Fatal("right half of a 980px page is missing on a phone capture")
	}
}

func TestBrowserElementCapture(t *testing.T) {
	cam := camera(t)
	srv := site()
	defer srv.Close()
	ctx := context.Background()

	o := opts("element")
	o.Selector, o.Padding = "#box", 10
	shot, err := cam.Capture(ctx, srv.URL+"/box", o)
	if err != nil {
		t.Fatal(err)
	}
	// 40.5..140.5 × 600.25..650.25, padded by 10 and rounded outward.
	if shot.CSSWidth != 121 || shot.CSSHeight != 71 {
		t.Fatalf("element box %dx%d", shot.CSSWidth, shot.CSSHeight)
	}
	o.Viewport.Scale = 2
	shot, err = cam.Capture(ctx, srv.URL+"/box", o)
	if err != nil {
		t.Fatal(err)
	}
	if w, h := pngSize(t, shot.Images[0]); w != 242 || h != 142 {
		t.Fatalf("2x element %dx%d", w, h)
	}
	o.Format = "jpeg"
	if shot, err = cam.Capture(ctx, srv.URL+"/box", o); err != nil || !bytes.HasPrefix(shot.Images[0], []byte{0xff, 0xd8, 0xff}) {
		t.Fatalf("jpeg: %v", err)
	}
	o.Format = "webp"
	if shot, err = cam.Capture(ctx, srv.URL+"/box", o); err != nil || string(shot.Images[0][8:12]) != "WEBP" {
		t.Fatalf("webp: %v", err)
	}

	for sel, want := range map[string]string{"[": "invalid selector: [", "#nope": "selector never appeared: #nope", "#zero": "no rendered size"} {
		o := opts("element")
		o.Selector, o.Timeout = sel, 3*time.Second
		if _, err := cam.Capture(ctx, srv.URL+"/box", o); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("selector %q: got %v, want %q", sel, err, want)
		}
	}
}
