package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

const userAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0 Safari/537.36"

var (
	// contentUrl fields appear inside a double-escaped JSON blob (Next.js __NEXT_DATA__),
	// so keys/values are wrapped in \" rather than ".
	reContentURLs    = regexp.MustCompile(`\\"(contentUrl(?:\d+p)?)\\":\\"([^\\]*(?:\\\\u[0-9a-fA-F]{4}[^\\]*)*)\\"`)
	reTitleEmbedded  = regexp.MustCompile(`\\"contentTitle\\":\\"([^\\]*(?:\\\\u[0-9a-fA-F]{4}[^\\]*)*)\\"`)
	reTitleJSONLD    = regexp.MustCompile(`"@type"\s*:\s*"VideoObject"[^}]*?"name"\s*:\s*"([^"]*)"`)
	reClipID         = regexp.MustCompile(`/clips/([A-Za-z0-9_-]+)`)
	reUnsafe         = regexp.MustCompile(`[<>:"/\\|?*\x00-\x1f]`)
	reQualityKey     = regexp.MustCompile(`^contentUrl(\d+)p$`)
)

type quality struct {
	label string // "source", "1080p", "720p", ...
	rank  int    // source=0 so it sorts first; otherwise the pixel height
	url   string
	size  int64
}

func fetchSize(url string) int64 {
	req, err := http.NewRequest("HEAD", url, nil)
	if err != nil {
		return 0
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Referer", "https://medal.tv/")

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return 0
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		return 0
	}
	return resp.ContentLength
}

func formatSize(n int64) string {
	if n <= 0 {
		return "unknown size"
	}
	return fmt.Sprintf("%.2f MiB", float64(n)/1048576)
}

type clip struct {
	title     string
	qualities []quality
}

func fetchPage(url string) (string, error) {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", userAgent)

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("HTTP %d fetching page", resp.StatusCode)
	}
	b, err := io.ReadAll(resp.Body)
	return string(b), err
}

// decodeDoubleEscaped converts a string from inside a double-escaped JSON blob
// (e.g. `https://...\\u0026t=720p`) to its real value.
func decodeDoubleEscaped(s string) (string, error) {
	// First unwrap one layer of backslash escaping, then parse as a JSON string.
	once := strings.ReplaceAll(s, `\\`, `\`)
	var out string
	if err := json.Unmarshal([]byte(`"`+once+`"`), &out); err != nil {
		return "", err
	}
	return out, nil
}

func extractClip(html string, getSize bool) (clip, error) {
	matches := reContentURLs.FindAllStringSubmatch(html, -1)
	if len(matches) == 0 {
		return clip{}, fmt.Errorf("no contentUrl fields found (clip may be private or page format changed)")
	}

	seen := map[string]bool{}
	var qs []quality
	for _, m := range matches {
		key := m[1]
		url, err := decodeDoubleEscaped(m[2])
		if err != nil {
			continue
		}
		// Skip transcodes flagged as missing — the CDN would just redirect
		// them back to the source file, so they are not distinct qualities.
		if strings.Contains(url, "&missing") {
			continue
		}
		if seen[key] {
			continue
		}
		seen[key] = true

		q := quality{url: url}
		if key == "contentUrl" {
			q.label = "source"
			q.rank = 0
		} else if km := reQualityKey.FindStringSubmatch(key); km != nil {
			q.label = km[1] + "p"
			n, _ := strconv.Atoi(km[1])
			q.rank = -n // higher resolution first (negative so ascending sort puts big numbers first)
		} else {
			q.label = key
		}
		qs = append(qs, q)
	}
	if len(qs) == 0 {
		return clip{}, fmt.Errorf("no usable qualities found on page")
	}
	sort.SliceStable(qs, func(i, j int) bool { return qs[i].rank < qs[j].rank })

	// Fetch sizes in parallel if requested
	if getSize {
		type sizeResult struct {
			index int
			size  int64
		}
		results := make(chan sizeResult, len(qs))
		for i, q := range qs {
			go func(i int, url string) {
				results <- sizeResult{i, fetchSize(url)}
			}(i, q.url)
		}
		for range qs {
			res := <-results
			qs[res.index].size = res.size
		}
	}

	title := ""
	if tm := reTitleEmbedded.FindStringSubmatch(html); tm != nil {
		if t, err := decodeDoubleEscaped(tm[1]); err == nil {
			title = t
		}
	}
	if title == "" {
		if tm := reTitleJSONLD.FindStringSubmatch(html); tm != nil {
			var t string
			if err := json.Unmarshal([]byte(`"`+tm[1]+`"`), &t); err == nil {
				title = t
			}
		}
	}
	return clip{title: title, qualities: qs}, nil
}

// uniquePath returns path as-is if no file exists there; otherwise inserts a
// "_YYYYMMDD-HHMMSS" timestamp before the extension.
func uniquePath(path string) string {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return path
	}
	ext := filepath.Ext(path)
	base := strings.TrimSuffix(path, ext)
	return fmt.Sprintf("%s_%s%s", base, time.Now().Format("20060102-150405"), ext)
}

func pickQuality(qs []quality, wanted string, in io.Reader, out io.Writer) (quality, error) {
	if wanted != "" {
		for _, q := range qs {
			if strings.EqualFold(q.label, wanted) {
				return q, nil
			}
		}
		avail := make([]string, len(qs))
		for i, q := range qs {
			avail[i] = q.label
		}
		return quality{}, fmt.Errorf("quality %q not available (have: %s)", wanted, strings.Join(avail, ", "))
	}
	if len(qs) == 1 {
		fmt.Fprintf(out, "Only one quality available: %s (%s)\n", qs[0].label, formatSize(qs[0].size))
		return qs[0], nil
	}
	fmt.Fprintln(out, "Available qualities:")
	for i, q := range qs {
		fmt.Fprintf(out, "  [%d] %-10s (%s)\n", i+1, q.label, formatSize(q.size))
	}
	fmt.Fprintf(out, "Choose [1-%d, default 1]: ", len(qs))
	line, _ := bufio.NewReader(in).ReadString('\n')
	line = strings.TrimSpace(line)
	if line == "" {
		return qs[0], nil
	}
	n, err := strconv.Atoi(line)
	if err != nil || n < 1 || n > len(qs) {
		return quality{}, fmt.Errorf("invalid selection %q", line)
	}
	return qs[n-1], nil
}

func safeFilename(name, fallback string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		name = fallback
	}
	name = reUnsafe.ReplaceAllString(name, "_")
	if len(name) > 120 {
		name = name[:120]
	}
	name = strings.TrimRight(name, " .")
	if name == "" {
		name = fallback
	}
	return name + ".mp4"
}

func clipIDFrom(url string) string {
	if m := reClipID.FindStringSubmatch(url); m != nil {
		return m[1]
	}
	return "clip"
}

type progressWriter struct {
	w          io.Writer
	total      int64
	done       int64
	lastReport time.Time
}

func (p *progressWriter) Write(b []byte) (int, error) {
	n, err := p.w.Write(b)
	p.done += int64(n)
	if time.Since(p.lastReport) > 100*time.Millisecond || err != nil {
		p.report()
		p.lastReport = time.Now()
	}
	return n, err
}

func (p *progressWriter) report() {
	mib := func(x int64) float64 { return float64(x) / 1048576 }
	if p.total > 0 {
		pct := float64(p.done) * 100 / float64(p.total)
		fmt.Printf("\r  %7.2f / %.2f MiB  (%5.1f%%)", mib(p.done), mib(p.total), pct)
	} else {
		fmt.Printf("\r  %7.2f MiB", mib(p.done))
	}
}

func download(url, dest string) error {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Referer", "https://medal.tv/")

	client := &http.Client{Timeout: 10 * time.Minute}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("HTTP %d downloading video", resp.StatusCode)
	}

	f, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer f.Close()

	pw := &progressWriter{w: f, total: resp.ContentLength}
	if _, err := io.Copy(pw, resp.Body); err != nil {
		fmt.Println()
		return err
	}
	pw.report()
	fmt.Println()
	return nil
}

func run() error {
	outDir := flag.String("o", ".", "directory to save into")
	name := flag.String("n", "", "override filename (without extension)")
	wanted := flag.String("q", "", "quality to pick without prompting (e.g. source, 720p, 1080p)")
	listOnly := flag.Bool("list", false, "list available qualities and exit")
	showSize := flag.Bool("s", true, "fetch and display video sizes")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: %s [-o DIR] [-n NAME] [-q QUALITY] [-s] [--list] <medal.tv clip URL>\n", os.Args[0])
		flag.PrintDefaults()
	}
	flag.Parse()
	if flag.NArg() != 1 {
		flag.Usage()
		return fmt.Errorf("exactly one URL required")
	}
	pageURL := flag.Arg(0)

	if err := os.MkdirAll(*outDir, 0o755); err != nil {
		return err
	}

	fmt.Printf("Fetching %s\n", pageURL)
	html, err := fetchPage(pageURL)
	if err != nil {
		return err
	}
	c, err := extractClip(html, *showSize)
	if err != nil {
		return err
	}

	title := c.title
	if title == "" {
		title = "(none)"
	}
	fmt.Printf("Title : %s\n", title)

	if *listOnly {
		fmt.Println("Qualities:")
		for _, q := range c.qualities {
			fmt.Printf("  %-10s (%s)\n", q.label, formatSize(q.size))
		}
		return nil
	}

	picked, err := pickQuality(c.qualities, *wanted, os.Stdin, os.Stdout)
	if err != nil {
		return err
	}

	base := *name
	if base == "" {
		base = c.title
	}
	if base == "" {
		base = clipIDFrom(pageURL)
	}
	if picked.label != "source" && *name == "" {
		base = base + "_" + picked.label
	}
	dest := uniquePath(filepath.Join(*outDir, safeFilename(base, clipIDFrom(pageURL))))

	displayURL := picked.url
	if i := strings.IndexByte(displayURL, '?'); i >= 0 {
		displayURL = displayURL[:i]
	}
	fmt.Printf("Quality: %s (%s)\n", picked.label, formatSize(picked.size))
	fmt.Printf("Video  : %s\n", displayURL)
	fmt.Printf("Saving : %s\n", dest)

	if err := download(picked.url, dest); err != nil {
		return err
	}
	fmt.Println("Done.")
	return nil
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
