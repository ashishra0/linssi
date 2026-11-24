package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/ashishra0/linssi"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// defaultMaxPixels is about what Claude keeps before downscaling an image
// (~1.15 MP, ~1,600 tokens). Other clients have similar caps.
const defaultMaxPixels = 1_150_000

const instructions = "Linssi is a camera for agents. Call capture for one page or element at a time. " +
	"It returns the image inline (tall full pages come back as top-to-bottom tiles) and writes a file only when output is set. " +
	"Read the warnings: a login redirect means pass storage_state or headers, or start the server with --connect."

// CaptureInput is the capture tool's argument schema.
type CaptureInput struct {
	URL              string            `json:"url" jsonschema:"URL to capture. Bare localhost and loopback hosts use http, other bare hosts use https."`
	Selector         string            `json:"selector,omitempty" jsonschema:"capture the first element matching this CSS selector"`
	Padding          *int              `json:"padding,omitempty" jsonschema:"CSS-pixel padding around the selected element"`
	FullPage         bool              `json:"full_page,omitempty" jsonschema:"capture the full page height; conflicts with selector"`
	Size             string            `json:"size,omitempty" jsonschema:"viewport as WxH or desktop, iphone, ipad (default desktop)"`
	Dark             bool              `json:"dark,omitempty" jsonschema:"emulate prefers-color-scheme: dark"`
	Format           string            `json:"format,omitempty" jsonschema:"png (default), jpg or webp; an output extension wins"`
	WaitMS           int               `json:"wait_ms,omitempty" jsonschema:"extra settle delay in ms after smart waiting"`
	WaitFor          string            `json:"wait_for,omitempty" jsonschema:"wait until this CSS selector exists before capturing"`
	ExpectText       string            `json:"expect_text,omitempty" jsonschema:"fail the capture unless this text (case-insensitive) shows on the page; waits for it, up to the timeout"`
	Scale            float64           `json:"scale,omitempty" jsonschema:"device scale factor overriding the viewport preset"`
	TimeoutSeconds   int               `json:"timeout_seconds,omitempty" jsonschema:"per-page timeout in seconds (default 30)"`
	Output           string            `json:"output,omitempty" jsonschema:"optional file path to also write; tiled captures write NAME-1.ext, NAME-2.ext, ..."`
	MaxPixels        *int64            `json:"max_pixels,omitempty" jsonschema:"max output pixels per image (default 1150000, sized for model context); tall full pages come back as tiles; 0 disables"`
	FreezeAnimations bool              `json:"freeze_animations,omitempty" jsonschema:"finish entrance animations and pause loops, videos and the caret, for repeatable shots"`
	Hide             []string          `json:"hide,omitempty" jsonschema:"CSS selectors to hide, such as cookie banners"`
	Mask             []string          `json:"mask,omitempty" jsonschema:"CSS selectors to cover with gray, such as timestamps"`
	Headers          map[string]string `json:"headers,omitempty" jsonschema:"extra request headers, such as Authorization"`
	StorageState     string            `json:"storage_state,omitempty" jsonschema:"path to a Playwright storage-state JSON or cookie array, for logged-in pages"`
}

// Prepare validates the input and turns it into a URL and capture options.
func (in CaptureInput) Prepare() (string, linssi.Options, error) {
	u, err := linssi.NormalizeURL(in.URL)
	if err != nil {
		return "", linssi.Options{}, err
	}
	sel := strings.TrimSpace(in.Selector)
	switch {
	case in.Selector != "" && sel == "":
		return "", linssi.Options{}, errors.New("selector must not be empty")
	case in.FullPage && sel != "":
		return "", linssi.Options{}, errors.New("selector conflicts with full_page")
	case in.Padding != nil && sel == "":
		return "", linssi.Options{}, errors.New("padding requires selector")
	case in.Padding != nil && *in.Padding < 0:
		return "", linssi.Options{}, errors.New("padding must not be negative")
	case in.TimeoutSeconds < 0:
		return "", linssi.Options{}, errors.New("timeout_seconds must be greater than zero")
	}
	vp, err := linssi.ParseViewport(in.Size, in.Scale)
	if err != nil {
		return "", linssi.Options{}, err
	}
	format := formatFromExt(in.Format)
	if in.Format != "" && format == "" {
		return "", linssi.Options{}, fmt.Errorf("unsupported format %q", in.Format)
	}
	if in.Output != "" {
		if format = formatFromExt(strings.ToLower(in.Output[strings.LastIndex(in.Output, ".")+1:])); format == "" {
			return "", linssi.Options{}, errors.New("output must end in .png, .jpg, .jpeg or .webp")
		}
	}
	if format == "" {
		format = "png"
	}
	timeout := 30
	if in.TimeoutSeconds > 0 {
		timeout = in.TimeoutSeconds
	}
	maxPixels := int64(defaultMaxPixels)
	if in.MaxPixels != nil {
		maxPixels = *in.MaxPixels
	}
	o := linssi.Options{
		Viewport: vp, Mode: "viewport", Dark: in.Dark, Format: format,
		Wait: time.Duration(in.WaitMS) * time.Millisecond, WaitFor: strings.TrimSpace(in.WaitFor),
		ExpectText: strings.TrimSpace(in.ExpectText),
		Timeout:    time.Duration(timeout) * time.Second, MaxPixels: maxPixels,
		Freeze: in.FreezeAnimations, Hide: in.Hide, Mask: in.Mask, Headers: in.Headers,
	}
	switch {
	case sel != "":
		o.Mode, o.Selector = "element", sel
		if in.Padding != nil {
			o.Padding = *in.Padding
		}
	case in.FullPage:
		o.Mode = "full_page"
	}
	if in.StorageState != "" {
		if o.Storage, err = linssi.LoadStorageState(in.StorageState); err != nil {
			return "", linssi.Options{}, err
		}
	}
	return u, o, nil
}

// Cameras keeps one warm browser for the server's lifetime and replaces it
// if it dies. At most 4 captures run at once.
// Cameras keeps one warm browser for the MCP server's lifetime, replaces it
// if it dies, and runs at most 4 captures at once. Extensions take captures
// through Shoot.
type Cameras struct {
	chrome, connect string
	mu              sync.Mutex
	cam             *linssi.Camera
	slots           chan struct{}
}

func (p *Cameras) get() (*linssi.Camera, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cam != nil && !p.cam.Alive() {
		p.cam.Close()
		p.cam = nil
	}
	if p.cam == nil {
		var err error
		if p.connect != "" {
			p.cam, err = linssi.Attach(p.connect)
		} else {
			p.cam, err = linssi.Launch(p.chrome)
		}
		if err != nil {
			return nil, err
		}
	}
	return p.cam, nil
}

func (p *Cameras) close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cam != nil {
		p.cam.Close()
		p.cam = nil
	}
}

// Shoot captures url with o on the shared browser, waiting for a free slot.
func (p *Cameras) Shoot(ctx context.Context, url string, o linssi.Options) (*linssi.Capture, error) {
	select {
	case p.slots <- struct{}{}:
		defer func() { <-p.slots }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	cam, err := p.get()
	if err != nil {
		return nil, err
	}
	return cam.Capture(ctx, url, o)
}

func (p *Cameras) capture(ctx context.Context, in CaptureInput) (*mcp.CallToolResult, any, error) {
	u, o, err := in.Prepare()
	if err != nil {
		return toolError(Report{Status: "error", URL: in.URL, Error: err.Error()}), nil, nil
	}
	shot, err := p.Shoot(ctx, u, o)
	if err != nil && ctx.Err() != nil {
		return nil, nil, ctx.Err()
	}
	if err == nil && in.Output != "" {
		err = WriteImages(in.Output, shot.Images)
	}
	if err != nil {
		return toolError(ErrorReport(u, in.Output, o, err)), nil, nil
	}

	report := SuccessReport(u, in.Output, o, shot)
	var b strings.Builder
	fmt.Fprintf(&b, "Captured %d×%d CSS px @%gx as %s", shot.CSSWidth, shot.CSSHeight, round2(shot.Scale), extFor(o.Format))
	if n := len(shot.Images); n > 1 {
		fmt.Fprintf(&b, " in %d top-to-bottom tiles", n)
	}
	if in.Output != "" {
		fmt.Fprintf(&b, " → %s", absPath(in.Output))
	}
	fmt.Fprintf(&b, "\nFinal URL: %s", shot.Page.FinalURL)
	if s := shot.Page.HTTPStatus; s != nil {
		fmt.Fprintf(&b, ", HTTP %d", *s)
	}
	fmt.Fprintf(&b, "\nTitle: %s", shot.Page.Title)
	for _, w := range shot.Page.Warnings {
		fmt.Fprintf(&b, "\nWarning: %s", w)
	}
	content := make([]mcp.Content, 0, len(shot.Images)+1)
	for _, img := range shot.Images {
		content = append(content, &mcp.ImageContent{Data: img, MIMEType: mimeFor(o.Format)})
	}
	content = append(content, &mcp.TextContent{Text: b.String()})
	return &mcp.CallToolResult{Content: content, StructuredContent: report}, nil, nil
}

func toolError(r Report) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		IsError:           true,
		Content:           []mcp.Content{&mcp.TextContent{Text: r.Error}},
		StructuredContent: r,
	}
}

func newServer(pool *Cameras, ext Extension) *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{Name: name, Version: Version}, &mcp.ServerOptions{Instructions: instructions})
	mcp.AddTool(server, &mcp.Tool{
		Name: "capture",
		Description: "Capture one trustworthy image of a live page or its first matching element. " +
			"Returns the image inline plus final URL, HTTP status and warnings (login redirect, blank page, " +
			"console errors, unsettled content) so you can judge the shot without guessing.",
		Annotations: &mcp.ToolAnnotations{Title: "Capture a web page", ReadOnlyHint: false, OpenWorldHint: ptr(true)},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in CaptureInput) (*mcp.CallToolResult, any, error) {
		return pool.capture(ctx, in)
	})
	if ext.Tools != nil {
		ext.Tools(server, pool)
	}
	return server
}

func runMCP(args []string, ext Extension) error {
	fs := flag.NewFlagSet("linssi mcp", flag.ContinueOnError)
	chrome := fs.String("chrome", os.Getenv("CHROME"), "browser binary (auto-detected; also $CHROME)")
	connect := fs.String("connect", "", "attach to a running Chrome (port, host:port or URL) so captures reuse its logins")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	pool := &Cameras{chrome: *chrome, connect: *connect, slots: make(chan struct{}, 4)}
	defer pool.close()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// On a signal, close the browser at once so in-flight captures fail fast
	// instead of holding the shutdown until they finish.
	context.AfterFunc(ctx, pool.close)
	err := newServer(pool, ext).Run(ctx, &mcp.StdioTransport{})
	if ctx.Err() != nil {
		return nil // stopped by a signal
	}
	return err
}
