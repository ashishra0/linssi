// Command bench captures a corpus of pages that break screenshot tools with
// each tool, and grades every capture: right, flagged, or silently wrong.
//
//	go run ./bench                    # local fixtures, Linssi and Iris
//	go run ./bench -live              # plus live sites (flaky by nature)
//	go run ./bench -tools linssi -only 'fade|spa'
package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"image/png"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/ashishra0/linssi"
)

//go:embed cases.json
var casesJSON []byte

type Case struct {
	Name     string `json:"name"`
	Path     string `json:"path,omitempty"` // local fixture
	URL      string `json:"url,omitempty"`  // live site
	Category string `json:"category"`
	Expect   string `json:"expect"` // "content": the real page; "warn": the page is wrong and the tool must say so
	Full     bool   `json:"full,omitempty"`
	Dark     bool   `json:"dark,omitempty"`
	Size     string `json:"size,omitempty"`
	Why      string `json:"why"`
}

// Grades. Only "silent" is a wrong image reported as fine.
const (
	pass       = "pass"        // right image, or a wrong page the tool flagged
	caught     = "caught"      // wrong image, but the tool flagged it
	falseAlarm = "false-alarm" // right image, but the tool flagged it
	failed     = "error"       // the tool failed on a page it should have captured
	silent     = "silent"      // wrong image reported as ok
)

type Result struct {
	Tool      string   `json:"tool"`
	Case      string   `json:"case"`
	Category  string   `json:"category"`
	Expect    string   `json:"expect"`
	Grade     string   `json:"grade"`
	Status    string   `json:"status"`
	Warnings  []string `json:"warnings,omitempty"`
	Error     string   `json:"error,omitempty"`
	Marker    bool     `json:"marker"`
	Blank     bool     `json:"blank"`
	Files     []string `json:"files,omitempty"`
	ElapsedMS int64    `json:"elapsed_ms"`
}

// report is the --json line both Linssi and Iris print.
type report struct {
	Status   string   `json:"status"`
	Output   string   `json:"output"`
	Files    []string `json:"files"`
	Warnings []string `json:"warnings"`
	Error    string   `json:"error"`
}

func main() {
	tools := flag.String("tools", "linssi,iris", "tools to run: linssi, iris, playwright")
	live := flag.Bool("live", false, "include live sites")
	only := flag.String("only", "", "run cases whose name matches this regexp")
	out := flag.String("out", filepath.Join(os.TempDir(), "linssi-bench"), "directory for images and results.json")
	chrome := flag.String("chrome", os.Getenv("CHROME"), "browser binary passed to every tool (default: $CHROME, else the one Linssi finds)")
	irisBin := flag.String("iris", "iris", "iris binary")
	flag.Parse()
	// Every tool gets the same explicit browser; left alone, each would pick its own.
	if *chrome == "" {
		*chrome = linssi.FindChrome()
	}
	if *chrome == "" {
		fatal(errors.New("no browser found: pass -chrome or set $CHROME"))
	}
	fmt.Fprintln(os.Stderr, "browser:", *chrome)

	var cases []Case
	if err := json.Unmarshal(casesJSON, &cases); err != nil {
		fatal(err)
	}
	re := regexp.MustCompile(*only)
	cases = slices.DeleteFunc(cases, func(c Case) bool { return (c.URL != "" && !*live) || !re.MatchString(c.Name) })

	if err := os.MkdirAll(*out, 0o755); err != nil {
		fatal(err)
	}
	bins := map[string]string{"iris": *irisBin}
	for _, t := range strings.Split(*tools, ",") {
		if t == "linssi" {
			bins[t] = filepath.Join(*out, "bin", "linssi")
			if b, err := exec.Command("go", "build", "-o", bins[t], "./cmd/linssi").CombinedOutput(); err != nil {
				fatal(fmt.Errorf("building linssi (run from the repo root): %v\n%s", err, b))
			}
		}
	}

	srv := httptest.NewServer(fixtures())
	defer srv.Close()

	var results []Result
	for _, t := range strings.Split(*tools, ",") {
		for _, c := range cases {
			u := c.URL
			if u == "" {
				u = srv.URL + c.Path
			}
			r := capture(t, bins[t], *chrome, u, c, filepath.Join(*out, t, c.Name+".png"))
			fmt.Fprintf(os.Stderr, "%-7s %-20s %-12s %5dms %s\n", t, c.Name, r.Grade, r.ElapsedMS, strings.Join(slices.DeleteFunc(append(r.Warnings, r.Error), func(s string) bool { return s == "" }), "; "))
			results = append(results, r)
		}
	}

	b, _ := json.MarshalIndent(map[string]any{"date": time.Now().Format(time.RFC3339), "cases": cases, "results": results}, "", "  ")
	os.WriteFile(filepath.Join(*out, "results.json"), b, 0o644)
	summarize(os.Stdout, strings.Split(*tools, ","), results)
	fmt.Println("\nimages and results.json:", *out)
}

func capture(tool, bin, chrome, u string, c Case, file string) Result {
	r := Result{Tool: tool, Case: c.Name, Category: c.Category, Expect: c.Expect}
	os.MkdirAll(filepath.Dir(file), 0o755)
	os.Remove(file)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	run := runCLI
	if tool == "playwright" {
		run = runPlaywright
	}
	rep, ms, err := run(ctx, bin, chrome, u, c, file)
	r.ElapsedMS = ms
	if err != nil {
		r.Error, r.Status, r.Grade = err.Error(), "crashed", failed // no report at all: a harness or tool crash, never a pass
		return r
	}
	r.Status, r.Warnings, r.Error = rep.Status, rep.Warnings, rep.Error
	r.Files = rep.Files
	if len(r.Files) == 0 && rep.Status == "ok" {
		r.Files = []string{rep.Output}
	}

	r.Blank = len(r.Files) > 0
	for _, f := range r.Files {
		m, blank := inspect(f)
		r.Marker = r.Marker || m
		r.Blank = r.Blank && blank
	}
	r.Grade = grade(c, r)
	return r
}

// runCLI runs Linssi or Iris, which share flags and the --json report.
func runCLI(ctx context.Context, bin, chrome, u string, c Case, file string) (report, int64, error) {
	args := []string{"--json", "--timeout", "30", "-o", file}
	args = append(args, "--chrome", chrome)
	if c.Full {
		args = append(args, "--full")
	}
	if c.Dark {
		args = append(args, "--dark")
	}
	if c.Size != "" {
		args = append(args, "--size", c.Size)
	}
	start := time.Now()
	stdout, err := exec.CommandContext(ctx, bin, append(args, u)...).Output()
	ms := time.Since(start).Milliseconds()
	var rep report
	if jerr := json.Unmarshal(lastLine(stdout), &rep); jerr != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return rep, ms, errors.New(strings.TrimSpace(string(ee.Stderr)))
		}
		return rep, ms, fmt.Errorf("%v %v", err, jerr)
	}
	return rep, ms, nil
}

func grade(c Case, r Result) string {
	flagged := r.Status != "ok" || len(r.Warnings) > 0
	if c.Expect == "warn" {
		if flagged {
			return pass
		}
		return silent
	}
	if r.Status != "ok" {
		return failed
	}
	right := r.Marker
	if c.URL != "" { // live pages have no marker; the best automatic check is "not blank"
		right = !r.Blank
	}
	switch {
	case right && !flagged:
		return pass
	case right:
		return falseAlarm
	case flagged:
		return caught
	}
	return silent
}

// inspect reports whether the image shows the fixture marker (a solid
// #ff00ff block of at least 10k device pixels) and whether it's nearly one
// color.
func inspect(file string) (marker, blank bool) {
	f, err := os.Open(file)
	if err != nil {
		return false, false
	}
	defer f.Close()
	img, err := png.Decode(f)
	if err != nil {
		return false, false
	}
	b := img.Bounds()
	hits, total, counts := 0, 0, map[[3]uint32]int{}
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			r, g, bl, _ := img.At(x, y).RGBA()
			r, g, bl = r>>8, g>>8, bl>>8
			if r >= 243 && g <= 12 && bl >= 243 {
				hits++
			}
			if x%4 == 0 && y%4 == 0 {
				counts[[3]uint32{r >> 4, g >> 4, bl >> 4}]++
				total++
			}
		}
	}
	top := 0
	for _, n := range counts {
		top = max(top, n)
	}
	return hits >= 10_000, total > 0 && float64(top)/float64(total) >= 0.985
}

func summarize(w *os.File, tools []string, results []Result) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', tabwriter.AlignRight)
	fmt.Fprintln(tw, "tool\tcases\tpass\tcaught\tfalse-alarm\terror\tsilent\tsilent rate\tmedian\tp95\t")
	for _, t := range tools {
		n, by, ms := 0, map[string]int{}, []int64{}
		for _, r := range results {
			if r.Tool == t {
				n++
				by[r.Grade]++
				ms = append(ms, r.ElapsedMS)
			}
		}
		if n == 0 {
			continue
		}
		slices.Sort(ms)
		fmt.Fprintf(tw, "%s\t%d\t%d\t%d\t%d\t%d\t%d\t%.1f%%\t%dms\t%dms\t\n", t, n, by[pass], by[caught], by[falseAlarm], by[failed], by[silent],
			100*float64(by[silent])/float64(n), ms[len(ms)/2], ms[min(len(ms)-1, len(ms)*95/100)])
	}
	tw.Flush()
	for _, t := range tools {
		var names []string
		for _, r := range results {
			if r.Tool == t && r.Grade == silent {
				names = append(names, r.Case)
			}
		}
		if len(names) > 0 {
			fmt.Fprintf(w, "\n%s silent: %s\n", t, strings.Join(names, ", "))
		}
	}
}

func lastLine(b []byte) []byte {
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	return []byte(lines[len(lines)-1])
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "bench:", err)
	os.Exit(1)
}
