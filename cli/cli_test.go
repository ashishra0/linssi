package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/ashishra0/linssi"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestParseArgs(t *testing.T) {
	a, err := parseArgs([]string{"example.com", "--full", "-H", "X-A: 1", "--hide", "#a", "--hide", ".b", "other.com"}, &bytes.Buffer{})
	if err != nil || len(a.urls) != 2 || !a.full || a.headers["X-A"] != "1" || len(a.hide) != 2 {
		t.Fatalf("interspersed flags: %+v %v", a, err)
	}
	for _, bad := range [][]string{
		{"x.com", "--selector", "h1", "--full"},
		{"x.com", "--padding", "4"},
		{"x.com", "--connect", "9222", "--storage-state", "a.json"},
		{"x.com", "-H", "nocolon"},
		{"x.com", "--format", "gif"},
		{"x.com", "--size", "big"},
		{"x.com", "-o", "shot.gif"},
		{"a.com", "b.com", "-o", "x.png"},
		{"x.com", "--timeout", "0"},
		{},
	} {
		_, err := parseArgs(bad, &bytes.Buffer{})
		var ue usageError
		if !errors.As(err, &ue) {
			t.Errorf("%v: want a usage error (exit 2), got %v", bad, err)
		}
	}
}

func TestNaming(t *testing.T) {
	if got := derivedName("https://example.com/pricing/"); got != "example.com-pricing" {
		t.Errorf("derivedName = %q", got)
	}
	if got := tilePath("shots/home.png", 2); got != "shots/home-2.png" {
		t.Errorf("tilePath = %q", got)
	}
	// Duplicate URLs must not take a tile's name.
	ts, _, err := resolveOutputs([]target{{url: "https://a.dev/"}, {url: "https://a.dev/"}}, t.TempDir(), "png")
	if err != nil || filepath.Base(ts[1].path) != "a.dev_2.png" {
		t.Errorf("duplicate name: %+v %v", ts, err)
	}
}

// site serves a tall page for the MCP test, and skips the test when no
// browser is installed.
func site(t *testing.T) *httptest.Server {
	t.Helper()
	cam, err := linssi.Launch("")
	if err != nil {
		t.Skipf("no browser: %v", err)
	}
	cam.Close()
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "<h1>hello</h1>") })
	mux.HandleFunc("/tall", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<style>body{margin:0}.hero{height:100vh;background:#00f}.rest{height:3760px;background:#f00}</style><div class="hero"></div><div class="rest">tall</div>`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestMCPCaptureTool(t *testing.T) {
	srv := site(t)
	ctx := context.Background()

	pool := &Cameras{slots: make(chan struct{}, 4)}
	defer pool.close()
	serverT, clientT := mcp.NewInMemoryTransports()
	if _, err := newServer(pool, Extension{}).Connect(ctx, serverT, nil); err != nil {
		t.Fatal(err)
	}
	session, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil).Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	tools, err := session.ListTools(ctx, nil)
	if err != nil || len(tools.Tools) != 1 || tools.Tools[0].Name != "capture" {
		t.Fatalf("tools: %+v %v", tools, err)
	}

	out := filepath.Join(t.TempDir(), "nested", "tall.png")
	res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "capture", Arguments: map[string]any{
		"url": srv.URL + "/tall", "full_page": true, "size": "320x240", "scale": 1, "max_pixels": 320 * 1000, "output": out,
	}})
	if err != nil || res.IsError {
		t.Fatalf("capture: %+v %v", res, err)
	}
	images := 0
	for _, c := range res.Content {
		if img, ok := c.(*mcp.ImageContent); ok {
			images++
			if img.MIMEType != "image/png" {
				t.Fatalf("mime %q", img.MIMEType)
			}
		}
	}
	var report map[string]any
	raw, _ := json.Marshal(res.StructuredContent)
	json.Unmarshal(raw, &report)
	if images != 4 || report["status"] != "ok" || report["tiles"] != float64(4) || len(report["files"].([]any)) != 4 {
		t.Fatalf("images %d, report %v", images, report)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(out), "tall-4.png")); err != nil {
		t.Fatal(err)
	}

	res, err = session.CallTool(ctx, &mcp.CallToolParams{Name: "capture", Arguments: map[string]any{"url": srv.URL, "selector": "h1", "full_page": true}})
	if err != nil || !res.IsError {
		t.Fatalf("conflict not rejected: %+v %v", res, err)
	}
}
