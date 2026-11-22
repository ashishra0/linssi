package linssi

import (
	"archive/zip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"github.com/chromedp/cdproto/browser"
	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/storage"
	"github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"
)

// Linssi's own browser is Chrome for Testing, kept in Linssi's cache folder.
// It has its own name and icon, so it is never mistaken for, or opened in
// place of, the browser someone uses every day.

const cftIndex = "https://googlechromelabs.github.io/chrome-for-testing/last-known-good-versions-with-downloads.json"

// cftPlatform is Chrome for Testing's name for this OS and CPU, and the
// binary's path inside its zip.
func cftPlatform() (platform, binary string, ok bool) {
	switch runtime.GOOS + "/" + runtime.GOARCH {
	case "darwin/arm64":
		return "mac-arm64", "chrome-mac-arm64/Google Chrome for Testing.app/Contents/MacOS/Google Chrome for Testing", true
	case "darwin/amd64":
		return "mac-x64", "chrome-mac-x64/Google Chrome for Testing.app/Contents/MacOS/Google Chrome for Testing", true
	case "linux/amd64":
		return "linux64", "chrome-linux64/chrome", true
	case "windows/amd64":
		return "win64", `chrome-win64\chrome.exe`, true
	}
	return "", "", false
}

func cftDir() (string, error) {
	base, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "linssi", "chrome-for-testing"), nil
}

// DedicatedChrome returns Linssi's installed Chrome for Testing, or "".
func DedicatedChrome() string {
	dir, err := cftDir()
	_, bin, ok := cftPlatform()
	if err != nil || !ok {
		return ""
	}
	p := filepath.Join(dir, filepath.FromSlash(bin))
	if _, err := os.Stat(p); err != nil {
		return ""
	}
	return p
}

// InstallDedicatedChrome downloads the current stable Chrome for Testing into
// Linssi's cache folder and returns its binary. progress gets one-line updates.
func InstallDedicatedChrome(ctx context.Context, progress io.Writer) (string, error) {
	if p := DedicatedChrome(); p != "" {
		return p, nil
	}
	platform, bin, ok := cftPlatform()
	if !ok {
		return "", fmt.Errorf("Chrome for Testing has no build for %s/%s; pass --chrome", runtime.GOOS, runtime.GOARCH)
	}
	dir, err := cftDir()
	if err != nil {
		return "", err
	}

	var index struct {
		Channels map[string]struct {
			Version   string `json:"version"`
			Downloads map[string][]struct {
				Platform string `json:"platform"`
				URL      string `json:"url"`
			} `json:"downloads"`
		} `json:"channels"`
	}
	if err := getJSON(ctx, cftIndex, &index); err != nil {
		return "", fmt.Errorf("reading the Chrome for Testing index: %w", err)
	}
	stable := index.Channels["Stable"]
	i := slices.IndexFunc(stable.Downloads["chrome"], func(d struct {
		Platform string `json:"platform"`
		URL      string `json:"url"`
	}) bool {
		return d.Platform == platform
	})
	if i < 0 {
		return "", fmt.Errorf("no Chrome for Testing download for %s", platform)
	}
	url := stable.Downloads["chrome"][i].URL
	fmt.Fprintf(progress, "Downloading Chrome for Testing %s (about 150 MB)...\n", stable.Version)

	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return "", err
	}
	zipFile, err := os.CreateTemp(filepath.Dir(dir), "cft-*.zip")
	if err != nil {
		return "", err
	}
	defer os.Remove(zipFile.Name())
	if err := download(ctx, url, zipFile); err != nil {
		zipFile.Close()
		return "", err
	}
	zipFile.Close()

	// Unpack beside the final folder, then rename, so a failed install leaves nothing half-done.
	tmp, err := os.MkdirTemp(filepath.Dir(dir), "cft-*")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp)
	fmt.Fprintln(progress, "Unpacking...")
	if err := unzip(zipFile.Name(), tmp); err != nil {
		return "", fmt.Errorf("unpacking Chrome for Testing: %w", err)
	}
	if err := os.Rename(tmp, dir); err != nil {
		return "", err
	}
	return filepath.Join(dir, filepath.FromSlash(bin)), nil
}

func getJSON(ctx context.Context, url string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: HTTP %d", url, res.StatusCode)
	}
	return json.NewDecoder(res.Body).Decode(v)
}

func download(ctx context.Context, url string, w io.Writer) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("downloading %s: HTTP %d", url, res.StatusCode)
	}
	_, err = io.Copy(w, res.Body)
	return err
}

// unzip extracts src into dest, keeping file modes and symlinks (a macOS app
// bundle needs both) and refusing paths that would escape dest.
func unzip(src, dest string) error {
	r, err := zip.OpenReader(src)
	if err != nil {
		return err
	}
	defer r.Close()
	root := filepath.Clean(dest) + string(os.PathSeparator)
	for _, f := range r.File {
		path := filepath.Join(dest, filepath.FromSlash(f.Name))
		if !strings.HasPrefix(path, root) {
			return fmt.Errorf("unsafe path in zip: %s", f.Name)
		}
		mode := f.Mode()
		if mode.IsDir() {
			if err := os.MkdirAll(path, 0o755); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		if mode&os.ModeSymlink != 0 {
			link, err := io.ReadAll(rc)
			rc.Close()
			if err != nil {
				return err
			}
			if filepath.IsAbs(string(link)) || !strings.HasPrefix(filepath.Join(filepath.Dir(path), string(link)), root) {
				return fmt.Errorf("unsafe symlink in zip: %s -> %s", f.Name, link)
			}
			if err := os.Symlink(string(link), path); err != nil {
				return err
			}
			continue
		}
		out, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode.Perm()|0o600)
		if err != nil {
			rc.Close()
			return err
		}
		_, err = io.Copy(out, rc)
		rc.Close()
		if cerr := out.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// BrowserVersion is the attached browser's product, like "Chrome/131.0.6778.85".
func (c *Camera) BrowserVersion() string {
	v, err := chromedp.CallBrowser(c.browserCtx, browser.GetVersion, cdp.Empty{})
	if err != nil {
		return ""
	}
	return v.Product
}

// Windows counts the browser's open pages (tabs).
func (c *Camera) Windows() (int, error) {
	res, err := chromedp.CallBrowser(c.browserCtx, target.GetTargets, target.GetTargetsParams{})
	if err != nil {
		return 0, err
	}
	n := 0
	for _, t := range res.TargetInfos {
		if t.Type == "page" {
			n++
		}
	}
	return n, nil
}

// OpenWindow opens url in a new window of the attached browser.
func (c *Camera) OpenWindow(url string) error {
	yes := true
	_, err := chromedp.CallBrowser(c.browserCtx, target.CreateTarget, target.CreateTargetParams{URL: url, NewWindow: &yes})
	return err
}

// Cookies returns every cookie in the attached browser.
func (c *Camera) Cookies() ([]*network.Cookie, error) {
	res, err := chromedp.CallBrowser(c.browserCtx, storage.GetCookies, storage.GetCookiesParams{})
	if err != nil {
		return nil, err
	}
	return res.Cookies, nil
}
