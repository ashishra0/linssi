package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// playwrightMCP is pinned so results don't move under us.
const playwrightMCP = "@playwright/mcp@0.0.85"

// runPlaywright captures the way an agent does with Playwright MCP:
// browser_navigate, then browser_take_screenshot. It reports no warnings, so
// a tool error is its only way to flag a page. The time covers the two calls
// (the browser launches on the first), not npx or Node starting.
func runPlaywright(ctx context.Context, _, chrome, u string, c Case, file string) (report, int64, error) {
	dir, err := os.MkdirTemp("", "bench-pw-")
	if err != nil {
		return report{}, 0, err
	}
	defer os.RemoveAll(dir)

	scheme := "light"
	if c.Dark {
		scheme = "dark"
	}
	opts := map[string]any{"viewport": map[string]int{"width": 1440, "height": 900}, "deviceScaleFactor": 2, "colorScheme": scheme}
	if c.Size == "iphone" {
		opts["viewport"], opts["deviceScaleFactor"], opts["isMobile"], opts["hasTouch"] = map[string]int{"width": 390, "height": 844}, 3, true, true
	}
	cfg, _ := json.Marshal(map[string]any{
		"browser":        map[string]any{"browserName": "chromium", "isolated": true, "contextOptions": opts},
		"imageResponses": "omit",
		"outputDir":      dir,
	})
	cfgPath := filepath.Join(dir, "config.json")
	os.WriteFile(cfgPath, cfg, 0o644)

	args := []string{"-y", playwrightMCP, "--config", cfgPath, "--headless", "--timeout-navigation", "30000"}
	args = append(args, "--executable-path", chrome)
	cmd := exec.Command("npx", args...)
	cmd.Dir = dir // the screenshot's relative path resolves against this
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "linssi-bench", Version: "0"}, nil).Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		return report{}, 0, err
	}
	defer cs.Close()

	start := time.Now()
	call := func(name string, args map[string]any) (string, bool, error) {
		res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
		if err != nil {
			return "", false, err
		}
		var text []string
		for _, ct := range res.Content {
			if t, ok := ct.(*mcp.TextContent); ok {
				text = append(text, t.Text)
			}
		}
		return strings.Join(text, "\n"), res.IsError, nil
	}
	rep := report{Status: "ok", Output: file}
	text, isErr, err := call("browser_navigate", map[string]any{"url": u})
	if err == nil && !isErr {
		text, isErr, err = call("browser_take_screenshot", map[string]any{"type": "png", "fullPage": c.Full, "filename": "shot.png"})
	}
	ms := time.Since(start).Milliseconds()
	if err != nil {
		return rep, ms, err
	}
	if isErr {
		rep.Status, rep.Error = "error", strings.ReplaceAll(strings.TrimSpace(text), "\n", " ")
		return rep, ms, nil
	}
	if err := os.Rename(filepath.Join(dir, "shot.png"), file); err != nil {
		return rep, ms, fmt.Errorf("%v: %s", err, firstLine(text))
	}
	return rep, ms, nil
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	return s
}
