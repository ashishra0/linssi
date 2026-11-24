// Package cli is the linssi command line and MCP server. cmd/linssi runs it;
// a build can add commands and MCP tools through an Extension.
package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/ashishra0/linssi"
	"github.com/chromedp/cdproto/network"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Version is set by the release build with -ldflags "-X github.com/ashishra0/linssi/cli.Version=...".
var Version = "dev"

const shortUsage = `Usage: linssi [flags] URL...    capture pages ("-" reads URLs from stdin)
       linssi mcp [--connect PORT] serve the capture tool to agents
       linssi browser              open Linssi's browser for logged-in captures
       linssi session export       save its logins to a file for --storage-state
Run 'linssi -h' for all flags and examples.
`

const help = `linssi — the camera for agents

Usage:
  linssi [flags] URL...        capture pages ("-" reads URLs from stdin)
  linssi mcp [--connect PORT]  serve the capture tool over MCP stdio
  linssi browser [--port N]    open Linssi's browser (its own profile) for logged-in captures
  linssi browser --install     download Chrome for Testing for it, once (~150 MB)
  linssi browser --status      show whether it is running
  linssi browser --stop        quit it (never any other browser)
  linssi session export [-o FILE] [--domain D]   save its logins for --storage-state

Examples:
  linssi example.com                          1440×900 @2x → example.com.png
  linssi --full --dark tailwindcss.com        full page, dark color scheme
  linssi --selector '#hero' --padding 24 app.dev
  linssi --connect 9222 localhost:3000/app    capture through 'linssi browser'
  linssi --storage-state auth.json app.dev    cookies + localStorage from a file
  linssi --full --max-pixels 1150000 app.dev  readable tiles sized for a model
  cat urls.txt | linssi - -o shots/           concurrent batch

Flags:
  -o, --out PATH             output file (one URL) or directory (default: .)
      --size SIZE            WxH[@Nx], or desktop (1440x900@2x), iphone (390x844@3x), ipad (1024x1366@2x)
      --full                 capture the full page height
      --selector CSS         capture the first element matching a CSS selector
      --padding PX           padding around --selector, in CSS px
      --dark                 emulate prefers-color-scheme: dark (default: light)
      --format FMT           png, jpg or webp (an --out extension wins; default png)
      --wait MS              extra delay after smart waiting
      --wait-for CSS         wait until a selector exists
      --expect-text TEXT     fail unless this text shows on the page (waits for it)
      --scale N              device scale factor (overrides the size preset)
      --max-pixels N         max output pixels per image; tall full pages split into tiles
      --freeze               finish entrance animations; pause loops, videos and the caret
      --hide CSS             hide matching elements (repeatable)
      --mask CSS             cover matching elements with gray (repeatable)
  -H, --header 'NAME: VALUE' extra request header (repeatable)
      --storage-state FILE   cookies + localStorage (Playwright format or cookie array)
      --connect TARGET       capture through a running Chrome: port, host:port or URL
      --chrome PATH          browser binary (auto-detected; also $CHROME)
      --jobs N               concurrent captures (default: min(4, URLs))
      --timeout SECS         per-page budget (default: 30)
      --json                 print one JSON object per finished capture
      --version              print the version
`

// usageError is a mistake in how linssi was called; it exits 2.
type usageError struct{ error }

func usageErrorf(format string, args ...any) error { return usageError{fmt.Errorf(format, args...)} }

type stringList []string

func (s *stringList) String() string     { return strings.Join(*s, ", ") }
func (s *stringList) Set(v string) error { *s = append(*s, v); return nil }

type headerList map[string]string

func (h headerList) String() string { return fmt.Sprint(map[string]string(h)) }
func (h headerList) Set(v string) error {
	name, value, ok := strings.Cut(v, ":")
	name = strings.TrimSpace(name)
	if !ok || name == "" || strings.ContainsAny(name, " \t") {
		return fmt.Errorf("expected 'Name: value', got %q", v)
	}
	h[name] = strings.TrimSpace(value)
	return nil
}

type cliArgs struct {
	urls       []string
	out        string
	size       string
	full       bool
	selector   string
	padding    int
	dark       bool
	format     string
	wait       int
	waitFor    string
	expectText string
	scale      float64
	jobs       int
	timeout    int
	chrome     string
	connect    string
	storage    string
	headers    headerList
	maxPixels  int64
	freeze     bool
	hide, mask stringList
	json       bool
	set        map[string]bool
}

func parseArgs(args []string, stderr io.Writer) (*cliArgs, error) {
	a := &cliArgs{headers: headerList{}, set: map[string]bool{}}
	fs := flag.NewFlagSet("linssi", flag.ContinueOnError)
	fs.SetOutput(io.Discard) // errors are printed once, by main
	fs.Usage = func() {}
	fs.StringVar(&a.out, "o", "", "output file (one URL) or directory (batch)")
	fs.StringVar(&a.out, "out", "", "same as -o")
	fs.StringVar(&a.size, "size", "desktop", "WxH, or desktop (1440x900@2x), iphone (390x844@3x), ipad (1024x1366@2x)")
	fs.BoolVar(&a.full, "full", false, "capture the full page height")
	fs.StringVar(&a.selector, "selector", "", "capture the first element matching this CSS selector")
	fs.IntVar(&a.padding, "padding", 0, "CSS-pixel padding around --selector")
	fs.BoolVar(&a.dark, "dark", false, "emulate prefers-color-scheme: dark")
	fs.StringVar(&a.format, "format", "", "png, jpg or webp (an --out extension wins) (default png)")
	fs.IntVar(&a.wait, "wait", 0, "extra settle delay in ms after smart waiting")
	fs.StringVar(&a.waitFor, "wait-for", "", "wait until this CSS selector exists")
	fs.StringVar(&a.expectText, "expect-text", "", "fail unless this text shows on the page")
	fs.Float64Var(&a.scale, "scale", 0, "device scale factor (overrides the preset)")
	fs.IntVar(&a.jobs, "jobs", 0, "concurrent captures (default min(4, URLs))")
	fs.IntVar(&a.timeout, "timeout", 30, "per-page budget in seconds")
	fs.StringVar(&a.chrome, "chrome", os.Getenv("CHROME"), "browser binary (auto-detected; also $CHROME)")
	fs.StringVar(&a.connect, "connect", "", "attach to a running Chrome: port, host:port or URL")
	fs.StringVar(&a.storage, "storage-state", "", "cookies + localStorage file (Playwright format or cookie array)")
	fs.Var(a.headers, "H", "extra request header 'Name: value' (repeatable)")
	fs.Var(a.headers, "header", "same as -H")
	fs.Int64Var(&a.maxPixels, "max-pixels", 0, "max output pixels per image; tall full pages split into tiles")
	fs.BoolVar(&a.freeze, "freeze", false, "finish entrance animations; pause loops, videos and the caret")
	fs.Var(&a.hide, "hide", "hide elements matching a CSS selector (repeatable)")
	fs.Var(&a.mask, "mask", "cover elements matching a CSS selector with gray (repeatable)")
	fs.BoolVar(&a.json, "json", false, "print one JSON object per finished capture")
	showVersion := fs.Bool("version", false, "print the version")

	// Allow flags after URLs: parse, take one positional, repeat.
	for {
		if err := fs.Parse(args); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return nil, err
			}
			return nil, usageError{err}
		}
		if fs.NArg() == 0 {
			break
		}
		a.urls = append(a.urls, fs.Arg(0))
		args = fs.Args()[1:]
	}
	fs.Visit(func(f *flag.Flag) { a.set[f.Name] = true })
	if *showVersion {
		fmt.Println(name, Version)
		os.Exit(0)
	}
	ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(a.out), "."))
	singleURL := len(a.urls) == 1 && a.urls[0] != "-"
	switch {
	case len(a.urls) == 0:
		return nil, usageError{errors.New("no URLs given")}
	case a.full && a.selector != "":
		return nil, usageErrorf("--selector conflicts with --full")
	case a.set["padding"] && a.selector == "":
		return nil, usageErrorf("--padding requires --selector")
	case a.padding < 0:
		return nil, usageErrorf("--padding must not be negative")
	case a.connect != "" && (a.set["chrome"] || a.storage != ""):
		return nil, usageErrorf("--connect uses that browser's own logins; it conflicts with --chrome and --storage-state")
	case a.format != "" && formatFromExt(a.format) == "":
		return nil, usageErrorf("unsupported --format %q: use png, jpg or webp", a.format)
	case formatFromExt(ext) != "" && !singleURL:
		return nil, usageErrorf("-o %s names one file; give a directory to capture several URLs", a.out)
	case notAnImageExt[ext]:
		return nil, usageErrorf("unsupported output extension .%s: use .png, .jpg or .webp", ext)
	case a.timeout <= 0:
		return nil, usageErrorf("--timeout must be greater than zero")
	}
	if _, err := linssi.ParseViewport(a.size, a.scale); err != nil {
		return nil, usageError{err}
	}
	return a, nil
}

// notAnImageExt lists output extensions people try that linssi can't write;
// any other extension is taken as part of a directory name.
var notAnImageExt = map[string]bool{"gif": true, "bmp": true, "tif": true, "tiff": true, "svg": true, "pdf": true, "avif": true, "heic": true, "ico": true}

// Extension adds commands and MCP tools to the CLI, for builds such as
// Linssi Pro. The zero value is plain linssi.
type Extension struct {
	Name     string                          // binary name in help and version output (default "linssi")
	Help     string                          // extra help text, printed after the flags
	Commands map[string]func([]string) error // extra subcommands, by name
	Tools    func(*mcp.Server, *Cameras)     // registers extra MCP tools
}

var name = "linssi"

// Main runs the CLI with os.Args and exits.
func Main(ext Extension) {
	if ext.Name != "" {
		name = ext.Name
	}
	if len(os.Args) > 1 {
		var err error
		ran := true
		switch cmd := os.Args[1]; {
		case cmd == "mcp":
			err = runMCP(os.Args[2:], ext)
		case cmd == "browser":
			err = runBrowser(os.Args[2:])
		case cmd == "session":
			err = runSession(os.Args[2:])
		case ext.Commands[cmd] != nil:
			err = ext.Commands[cmd](os.Args[2:])
		default:
			ran = false
		}
		if ran {
			if err != nil {
				fmt.Fprintf(os.Stderr, "%s: %v\n", name, err)
				var ue usageError
				if errors.As(err, &ue) {
					os.Exit(2)
				}
				os.Exit(1)
			}
			return
		}
	}
	if len(os.Args) == 1 {
		fmt.Fprint(os.Stderr, shortUsage)
		os.Exit(2)
	}
	a, err := parseArgs(os.Args[1:], os.Stderr)
	if errors.Is(err, flag.ErrHelp) {
		fmt.Print(help + ext.Help)
		return
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\nRun '%s -h' for help.\n", name, err, name)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	failed, err := run(ctx, a)
	interrupted := ctx.Err() != nil // read before stop(), which cancels ctx
	stop()
	switch {
	case interrupted:
		os.Exit(130) // interrupted; run already closed the browser and its profile
	case errors.As(err, new(usageError)):
		fmt.Fprintf(os.Stderr, "%s: %v\nRun '%s -h' for help.\n", name, err, name)
		os.Exit(2)
	case err != nil:
		fmt.Fprintf(os.Stderr, "%s: %v\n", name, err)
		os.Exit(1)
	case failed > 0:
		os.Exit(1)
	}
}

// UsageError marks a mistake in how a command was called; Main exits 2.
func UsageError(format string, args ...any) error { return usageErrorf(format, args...) }

func ptr[T any](v T) *T { return &v }

func run(ctx context.Context, a *cliArgs) (int, error) {
	urls, err := collectURLs(a.urls, os.Stdin)
	if err != nil {
		return 0, err
	}
	vp, err := linssi.ParseViewport(a.size, a.scale)
	if err != nil {
		return 0, err
	}
	targets, format, err := resolveOutputs(urls, a.out, formatFromExt(a.format))
	if err != nil {
		return 0, err
	}
	opts := linssi.Options{
		Viewport: vp, Mode: "viewport", Selector: a.selector, Padding: a.padding, Dark: a.dark,
		Wait: time.Duration(a.wait) * time.Millisecond, WaitFor: a.waitFor, ExpectText: a.expectText,
		Timeout: time.Duration(max(a.timeout, 1)) * time.Second, Format: format,
		MaxPixels: a.maxPixels, Freeze: a.freeze, Hide: a.hide, Mask: a.mask, Headers: a.headers,
	}
	switch {
	case a.selector != "":
		opts.Mode = "element"
	case a.full:
		opts.Mode = "full_page"
	}
	if a.storage != "" {
		if opts.Storage, err = linssi.LoadStorageState(a.storage); err != nil {
			return 0, err
		}
	}

	var cam *linssi.Camera
	if a.connect != "" {
		cam, err = linssi.Attach(a.connect)
	} else {
		cam, err = linssi.Launch(a.chrome)
	}
	if err != nil {
		return 0, err
	}
	defer cam.Close()

	jobs := a.jobs
	if jobs <= 0 {
		jobs = min(4, len(targets))
	}
	var (
		mu     sync.Mutex
		failed int
		wg     sync.WaitGroup
		slots  = make(chan struct{}, jobs)
	)
	for _, t := range targets {
		wg.Go(func() {
			slots <- struct{}{}
			defer func() { <-slots }()
			var shot *linssi.Capture
			err := t.err // a line that isn't a URL fails alone, not the batch
			if err == nil {
				shot, err = cam.Capture(ctx, t.url, opts)
			}
			if err == nil {
				err = WriteImages(t.path, shot.Images)
			}
			if ctx.Err() != nil {
				return // interrupted: say nothing more
			}
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				failed++
				if a.json {
					printJSON(ErrorReport(t.url, t.path, opts, err))
				} else {
					fmt.Fprintf(os.Stderr, "✗ %s — %v\n", t.url, err)
				}
				return
			}
			if a.json {
				printJSON(SuccessReport(t.url, t.path, opts, shot))
				return
			}
			name := t.path
			if n := len(shot.Images); n > 1 {
				// Name the files that were really written.
				name = fmt.Sprintf("%s … %s (%d tiles)", tilePath(t.path, 1), filepath.Base(tilePath(t.path, n)), n)
			}
			fmt.Printf("✓ %s — %d×%d @%gx, %s\n", name, shot.CSSWidth, shot.CSSHeight,
				round2(shot.Scale), humanSize(shot.Bytes()))
			if final := shot.Page.FinalURL; final != "" && final != t.url {
				fmt.Printf("  → ended at %s\n", final)
			}
			for _, w := range shot.Page.Warnings {
				fmt.Fprintf(os.Stderr, "  ⚠ %s\n", w)
			}
		})
	}
	wg.Wait()
	return failed, nil
}

// Report is the JSON shape of one capture, shared by the CLI and MCP.
type Report struct {
	Status    string   `json:"status"`
	URL       string   `json:"url"`
	Output    string   `json:"output,omitempty"`
	Mode      string   `json:"mode"`
	Selector  string   `json:"selector,omitempty"`
	Padding   *int     `json:"padding,omitempty"`
	CSSWidth  int      `json:"css_width,omitempty"`
	CSSHeight int      `json:"css_height,omitempty"`
	Scale     float64  `json:"scale,omitempty"`
	Format    string   `json:"format,omitempty"`
	Bytes     int      `json:"bytes,omitempty"`
	Tiles     int      `json:"tiles,omitempty"`
	Files     []string `json:"files,omitempty"`
	Error     string   `json:"error,omitempty"`
	*linssi.PageInfo
}

func baseReport(status, u, out string, o linssi.Options) Report {
	r := Report{Status: status, URL: u, Output: absPath(out), Mode: o.Mode}
	if o.Mode == "element" {
		r.Selector, r.Padding = o.Selector, ptr(o.Padding)
	}
	return r
}

// SuccessReport is the JSON report of a finished capture.
func SuccessReport(u, out string, o linssi.Options, c *linssi.Capture) Report {
	r := baseReport("ok", u, out, o)
	r.CSSWidth, r.CSSHeight, r.Scale = c.CSSWidth, c.CSSHeight, round2(c.Scale)
	r.Format, r.Bytes, r.Tiles = extFor(o.Format), c.Bytes(), len(c.Images)
	if out != "" && len(c.Images) > 1 {
		for i := range c.Images {
			r.Files = append(r.Files, absPath(tilePath(out, i+1)))
		}
	}
	page := c.Page
	r.PageInfo = &page
	return r
}

// ErrorReport is the JSON report of a failed capture.
func ErrorReport(u, out string, o linssi.Options, err error) Report {
	r := baseReport("error", u, out, o)
	r.Error = err.Error()
	return r
}

func printJSON(v any) {
	b, _ := json.Marshal(v)
	fmt.Println(string(b))
}

// collectURLs reads the URL arguments and stdin lines. A line that isn't a
// URL comes back with its error, so it fails alone instead of the batch.
func collectURLs(args []string, stdin io.Reader) ([]target, error) {
	var raw []string
	for _, a := range args {
		if a != "-" {
			raw = append(raw, a)
			continue
		}
		sc := bufio.NewScanner(stdin)
		for sc.Scan() {
			if line := strings.TrimSpace(sc.Text()); line != "" && !strings.HasPrefix(line, "#") {
				raw = append(raw, line)
			}
		}
		if err := sc.Err(); err != nil {
			return nil, fmt.Errorf("reading URLs from stdin: %w", err)
		}
	}
	if len(raw) == 0 {
		return nil, errors.New("no URLs given")
	}
	urls := make([]target, len(raw))
	for i, r := range raw {
		u, err := linssi.NormalizeURL(r)
		if err != nil {
			urls[i] = target{url: r, err: err}
		} else {
			urls[i] = target{url: u}
		}
	}
	return urls, nil
}

type target struct {
	url, path string
	err       error
}

// resolveOutputs pairs URLs with files. One URL plus "--out file.ext" writes
// that file and its extension sets the format; otherwise --out (default ".")
// is a directory of names derived from each URL.
func resolveOutputs(urls []target, out, flagFormat string) ([]target, string, error) {
	if f := formatFromExt(filepath.Ext(out)); f != "" {
		if len(urls) != 1 {
			return nil, "", usageErrorf("-o %s names one file; give a directory to capture several URLs", out)
		}
		if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
			return nil, "", err
		}
		urls[0].path = out
		return urls, f, nil
	}
	format := flagFormat
	if format == "" {
		format = "png"
	}
	dir := out
	if dir == "" {
		dir = "."
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, "", err
	}
	seen := map[string]int{}
	for i, t := range urls {
		name := derivedName(t.url)
		seen[name]++
		// "_2", not "-2": tiles already use name-1, name-2, ...
		if n := seen[name]; n > 1 {
			name = fmt.Sprintf("%s_%d", name, n)
		}
		urls[i].path = filepath.Join(dir, name+"."+extFor(format))
	}
	return urls, format, nil
}

var unsafeChars = regexp.MustCompile(`[^A-Za-z0-9._-]`)

// derivedName turns https://example.com/pricing/ into example.com-pricing.
func derivedName(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "page"
	}
	name := u.Hostname()
	if name == "" {
		name = strings.TrimSuffix(filepath.Base(u.Path), filepath.Ext(u.Path))
	} else if p := strings.Trim(u.Path, "/"); p != "" {
		name += "-" + strings.ReplaceAll(p, "/", "-")
	}
	if name == "" {
		name = "page"
	}
	return unsafeChars.ReplaceAllString(name, "-")
}

func formatFromExt(ext string) string {
	switch strings.ToLower(strings.TrimPrefix(ext, ".")) {
	case "png":
		return "png"
	case "jpg", "jpeg":
		return "jpeg"
	case "webp":
		return "webp"
	}
	return ""
}

func extFor(format string) string {
	if format == "jpeg" {
		return "jpg"
	}
	return format
}

func mimeFor(format string) string { return "image/" + format }

// tilePath maps shots/home.png and tile 2 to shots/home-2.png.
func tilePath(out string, i int) string {
	ext := filepath.Ext(out)
	return fmt.Sprintf("%s-%d%s", strings.TrimSuffix(out, ext), i, ext)
}

// WriteImages writes one image to out, or tiles to out-1, out-2, ...
func WriteImages(out string, images [][]byte) error {
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		return err
	}
	if len(images) == 1 {
		return os.WriteFile(out, images[0], 0o644)
	}
	for i, img := range images {
		if err := os.WriteFile(tilePath(out, i+1), img, 0o644); err != nil {
			return err
		}
	}
	return nil
}

func absPath(p string) string {
	if p == "" {
		return ""
	}
	if a, err := filepath.Abs(p); err == nil {
		return a
	}
	return p
}

func round2(f float64) float64 { return float64(int(f*100+0.5)) / 100 }

func humanSize(n int) string {
	if n >= 1<<20 {
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	}
	return fmt.Sprintf("%d KB", (n+1023)/1024)
}

// browserProfile is where 'linssi browser' keeps its logins.
func browserProfile() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "linssi", "browser-profile"), nil
}

// runBrowser opens a visible browser with a profile of its own and a DevTools
// port, so people sign in to their apps once and agents capture through it.
// Chrome 136+ refuses remote debugging on the default profile, which is why
// "attach to the Chrome you already use" needs this separate profile.
//
// It runs Linssi's own Chrome for Testing and never picks a browser on its
// own: a second copy of someone's everyday browser looks just like theirs, so
// its windows get mixed up with theirs and closed by mistake.
func runBrowser(args []string) error {
	fs := flag.NewFlagSet("linssi browser", flag.ContinueOnError)
	port := fs.Int("port", 9222, "DevTools port to listen on")
	chrome := fs.String("chrome", os.Getenv("CHROME"), "use this browser binary instead of Linssi's Chrome for Testing (also $CHROME)")
	install := fs.Bool("install", false, "download Chrome for Testing for Linssi to use (about 150 MB, once)")
	status := fs.Bool("status", false, "show whether Linssi's browser is running")
	stop := fs.Bool("stop", false, "quit Linssi's browser (never any other browser)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	profile, err := browserProfile()
	if err != nil {
		return err
	}
	next := fmt.Sprintf(`
Sign in to your apps in that window once; the profile keeps the logins.
Capture through it:       linssi --connect %[1]d localhost:3000/app
Give it to Claude Code:   claude mcp add linssi -- linssi mcp --connect %[1]d
Quit it later:            linssi browser --stop
`, *port)

	if cam, err := linssi.Attach(strconv.Itoa(*port)); err == nil {
		defer cam.Close()
		ours := launchedHere(profile, *port)
		windows, _ := cam.Windows()
		switch {
		case *status && ours:
			fmt.Printf("Linssi's browser is running on port %d: %s, %d open tab(s).\nProfile: %s\n", *port, cam.BrowserVersion(), windows, profile)
			return nil
		case *status:
			fmt.Printf("Port %d has a browser that isn't Linssi's (%s).\n", *port, cam.BrowserVersion())
			return nil
		case !ours:
			// Never close, reuse or open windows in a browser Linssi didn't start.
			return fmt.Errorf("port %d belongs to a browser that isn't Linssi's; leaving it alone. Use --port for another port", *port)
		case *stop:
			if err := quitLaunched(profile); err != nil {
				return err
			}
			os.Remove(launchFile(profile))
			fmt.Printf("Quit Linssi's browser on port %d. Its logins stay in %s.\n", *port, profile)
			return nil
		case windows == 0:
			// On macOS a browser keeps running after its last window closes.
			if err := cam.OpenWindow("about:blank"); err != nil {
				return err
			}
			fmt.Printf("Linssi's browser was running with no window; opened a new one.\n%s", next)
			return nil
		default:
			fmt.Printf("Linssi's browser is already running on port %d.\n%s", *port, next)
			return nil
		}
	}
	if *status || *stop {
		fmt.Printf("Linssi's browser isn't running on port %d.\n", *port)
		return nil
	}

	path := *chrome
	if path == "" {
		path = linssi.DedicatedChrome()
	}
	if path == "" && *install {
		ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer cancel()
		if path, err = linssi.InstallDedicatedChrome(ctx, os.Stdout); err != nil {
			return err
		}
	}
	if path == "" {
		return usageErrorf(`Linssi's browser isn't installed.
  linssi browser --install          download Chrome for Testing for Linssi (about 150 MB, once)
  linssi browser --chrome PATH      or use a browser you choose
Linssi doesn't pick one of your browsers itself: a second copy of the browser you use every day looks just like it.`)
	}
	if path != linssi.DedicatedChrome() {
		fmt.Printf("Using %s. This opens a separate copy with Linssi's own profile; it can look like your usual window.\n", filepath.Base(path))
	}
	if err := os.MkdirAll(profile, 0o700); err != nil {
		return err
	}
	cmd := exec.Command(path, fmt.Sprintf("--remote-debugging-port=%d", *port), "--user-data-dir="+profile,
		"--no-first-run", "--no-default-browser-check")
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting %s: %w", path, err)
	}
	rec, _ := json.Marshal(launch{PID: cmd.Process.Pid, Port: *port})
	if err := os.WriteFile(launchFile(profile), rec, 0o600); err != nil {
		return err
	}
	fmt.Printf("Opened %s with the Linssi profile on port %d.\nProfile: %s\n%s", filepath.Base(path), *port, profile, next)
	return nil
}

// launch records the browser 'linssi browser' started, so later calls can
// tell it apart from any other browser on the same port.
type launch struct {
	PID  int `json:"pid"`
	Port int `json:"port"`
}

func launchFile(profile string) string { return filepath.Join(filepath.Dir(profile), "browser.json") }

// launchedHere reports whether the browser on port is the one 'linssi browser'
// started: that process is still alive and was launched on this port. Only one
// process can listen on a port, so a live match is that browser.
// ponytail: trusts the PID; a reused PID with a foreign browser on the same port would fool it.
func launchedHere(profile string, port int) bool {
	data, err := os.ReadFile(launchFile(profile))
	var l launch
	if err != nil || json.Unmarshal(data, &l) != nil || l.Port != port || l.PID <= 0 {
		return false
	}
	p, err := os.FindProcess(l.PID)
	if err != nil {
		return false
	}
	if runtime.GOOS == "windows" {
		return true // FindProcess fails on Windows when the process is gone
	}
	return p.Signal(syscall.Signal(0)) == nil
}

// quitLaunched asks the browser 'linssi browser' started to quit, the way
// quitting from its menu would, and waits for it to exit.
// ponytail: Windows has no graceful signal here, so it ends the process; use a window-close message if that loses state.
func quitLaunched(profile string) error {
	data, err := os.ReadFile(launchFile(profile))
	if err != nil {
		return err
	}
	var l launch
	if err := json.Unmarshal(data, &l); err != nil {
		return err
	}
	p, err := os.FindProcess(l.PID)
	if err != nil {
		return err
	}
	if runtime.GOOS == "windows" {
		return p.Kill()
	}
	if err := p.Signal(syscall.SIGTERM); err != nil {
		return err
	}
	for range 50 {
		if p.Signal(syscall.Signal(0)) != nil {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("Linssi's browser (pid %d) didn't quit within 5 s", l.PID)
}

// runSession saves the signed-in session from Linssi's browser as a storage
// state file, so CI and headless captures can use it with --storage-state.
func runSession(args []string) error {
	if len(args) == 0 || args[0] != "export" {
		return usageErrorf("usage: linssi session export [-o FILE] [--domain DOMAIN]... [--connect PORT]")
	}
	fs := flag.NewFlagSet("linssi session export", flag.ContinueOnError)
	out := fs.String("o", "linssi-session.json", "file to write")
	connect := fs.String("connect", "9222", "Linssi's browser: port, host:port or URL")
	var domains stringList
	fs.Var(&domains, "domain", "only export cookies for this site and its subdomains (repeatable)")
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	cam, err := linssi.Attach(*connect)
	if err != nil {
		return err
	}
	defer cam.Close()
	cookies, err := cam.Cookies()
	if err != nil {
		return err
	}
	data, n := storageState(cookies, domains)
	if n == 0 {
		return errors.New("no cookies to export; sign in to the site in Linssi's browser first (linssi browser)")
	}
	if err := os.WriteFile(*out, data, 0o600); err != nil {
		return err
	}
	fmt.Printf("Wrote %d cookie(s) to %s. It holds your logins: keep it private and out of git.\nUse it with:  linssi --storage-state %s URL\n", n, *out, *out)
	return nil
}

// storageState writes cookies as a Playwright storage state, keeping only
// cookies for the given domains (all of them when none are given).
// ponytail: cookies only; localStorage logins need a page visit per origin, add if a site needs it.
func storageState(cookies []*network.Cookie, domains []string) ([]byte, int) {
	type cookie struct {
		Name     string  `json:"name"`
		Value    string  `json:"value"`
		Domain   string  `json:"domain"`
		Path     string  `json:"path"`
		Expires  float64 `json:"expires"`
		HTTPOnly bool    `json:"httpOnly"`
		Secure   bool    `json:"secure"`
		SameSite string  `json:"sameSite,omitempty"`
	}
	keep := func(d string) bool {
		if len(domains) == 0 {
			return true
		}
		d = strings.TrimPrefix(strings.ToLower(d), ".")
		for _, want := range domains {
			want = strings.TrimPrefix(strings.ToLower(want), ".")
			if d == want || strings.HasSuffix(d, "."+want) {
				return true
			}
		}
		return false
	}
	state := struct {
		Cookies []cookie `json:"cookies"`
		Origins []any    `json:"origins"`
	}{Cookies: []cookie{}, Origins: []any{}}
	for _, c := range cookies {
		if !keep(c.Domain) {
			continue
		}
		exp := c.Expires
		if c.Session {
			exp = -1
		}
		state.Cookies = append(state.Cookies, cookie{c.Name, c.Value, c.Domain, c.Path, exp, c.HTTPOnly, c.Secure, string(c.SameSite)})
	}
	data, _ := json.MarshalIndent(state, "", "  ")
	return data, len(state.Cookies)
}
