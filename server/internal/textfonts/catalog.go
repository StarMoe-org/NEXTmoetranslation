// Package textfonts reads the operator-selected immutable font catalogue.
// It never downloads URLs supplied by an editor or embeds font bytes in drafts.
package textfonts

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
)

type Coverage [][2]int

func (c *Coverage) UnmarshalJSON(raw []byte) error {
	var values [][]int
	if err := json.Unmarshal(raw, &values); err != nil {
		return err
	}
	if values == nil || len(values) > 65536 {
		return fmt.Errorf("invalid font coverage")
	}
	result := make(Coverage, len(values))
	for index, row := range values {
		if len(row) != 2 {
			return fmt.Errorf("invalid font coverage pair")
		}
		result[index] = [2]int{row[0], row[1]}
	}
	if err := result.validate(); err != nil {
		return err
	}
	*c = result
	return nil
}

type Reference struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Bytes  int    `json:"bytes"`
}
type BaseFont struct {
	Coverage Coverage `json:"coverage"`
}
type Font struct {
	Reference
	Coverage Coverage  `json:"coverage"`
	License  Reference `json:"license"`
}
type Catalog struct {
	Schema    string              `json:"schema"`
	Revision  string              `json:"revision"`
	BaseFonts map[string]BaseFont `json:"baseFonts"`
	Fonts     []Font              `json:"fonts"`
}

var hash = regexp.MustCompile(`^[a-f0-9]{64}$`)

func (c Coverage) validate() error {
	if len(c) > 65536 {
		return fmt.Errorf("font coverage exceeds limit")
	}
	previous := -1
	for _, r := range c {
		if r[0] < 0 || r[1] > 0x10ffff || r[0] > r[1] || r[0] <= previous || (r[0] <= 0xdfff && r[1] >= 0xd800) {
			return fmt.Errorf("invalid font coverage")
		}
		previous = r[1]
	}
	return nil
}
func (c Coverage) Has(value rune) bool {
	i := sort.Search(len(c), func(i int) bool { return c[i][1] >= int(value) })
	return i < len(c) && c[i][0] <= int(value)
}
func reference(r Reference, extension string, maximum int) bool {
	return hash.MatchString(r.SHA256) && r.Path == "objects/"+r.SHA256+extension && r.Bytes > 0 && r.Bytes <= maximum
}
func Parse(raw []byte) (Catalog, error) {
	var c Catalog
	if len(raw) > 2<<20 || json.Unmarshal(raw, &c) != nil || c.Schema != "moly-text-fonts/1" || !hash.MatchString(c.Revision) || len(c.BaseFonts) == 0 || len(c.BaseFonts) > 64 || len(c.Fonts) == 0 || len(c.Fonts) > 8 {
		return c, fmt.Errorf("invalid font catalogue")
	}
	for id, font := range c.BaseFonts {
		if !hash.MatchString(id) || font.Coverage.validate() != nil {
			return c, fmt.Errorf("invalid base font coverage")
		}
	}
	seen := map[string]bool{}
	for _, font := range c.Fonts {
		if !reference(font.Reference, ".ttf", 32<<20) || !reference(font.License, ".txt", 256<<10) || font.Coverage.validate() != nil || seen[font.SHA256] {
			return c, fmt.Errorf("invalid supplemental font")
		}
		seen[font.SHA256] = true
	}
	return c, nil
}

// Missing uses supplemental coverage only: a draft must be renderable by every
// advertised engine, not merely a union of unrelated base font subsets.
func (c Catalog) Missing(text string) []string {
	points := map[rune]bool{}
	for _, r := range text {
		if unicode.IsControl(r) || r == 0x61c || r == 0x200b || r == 0x200e || r == 0x200f || r == 0x2060 {
			continue
		}
		found := false
		for _, font := range c.Fonts {
			if font.Coverage.Has(r) {
				found = true
				break
			}
		}
		if !found {
			points[r] = true
		}
	}
	sorted := make([]int, 0, len(points))
	for r := range points {
		sorted = append(sorted, int(r))
	}
	sort.Ints(sorted)
	out := []string{}
	for _, r := range sorted {
		if len(out) == 16 {
			break
		}
		out = append(out, fmt.Sprintf("U+%04X", r))
	}
	return out
}

type Loader struct {
	url    *url.URL
	client *http.Client
	mu     sync.Mutex
	cached *Catalog
	at     time.Time
}

func New(raw string) (*Loader, error) {
	u, e := url.Parse(raw)
	if e != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (!strings.HasSuffix(u.Path, "/latest.json") && !regexp.MustCompile(`/revisions/[a-f0-9]{64}\.json$`).MatchString(u.Path)) {
		return nil, fmt.Errorf("MYSEKAI_FONT_CATALOG_URL must be a fixed HTTPS latest or revision URL")
	}
	return &Loader{url: u, client: &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return fmt.Errorf("font catalogue redirects are refused") }}}, nil
}
func (l *Loader) read(ctx context.Context, u *url.URL) ([]byte, error) {
	req, e := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if e != nil {
		return nil, e
	}
	req.Header.Set("Cache-Control", "no-cache")
	res, e := l.client.Do(req)
	if e != nil {
		return nil, e
	}
	defer res.Body.Close()
	if res.StatusCode != 200 || !strings.Contains(res.Header.Get("Content-Type"), "application/json") {
		return nil, fmt.Errorf("font catalogue unavailable")
	}
	raw, e := io.ReadAll(io.LimitReader(res.Body, (2<<20)+1))
	if e != nil {
		return nil, e
	}
	if len(raw) > 2<<20 {
		return nil, fmt.Errorf("font catalogue exceeds limit")
	}
	return raw, nil
}
func (l *Loader) Load(ctx context.Context) (Catalog, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.cached != nil && time.Since(l.at) < 30*time.Second {
		return *l.cached, nil
	}
	raw, e := l.read(ctx, l.url)
	if e != nil {
		return Catalog{}, e
	}
	c, e := Parse(raw)
	if e != nil {
		return c, e
	}
	if strings.HasSuffix(l.url.Path, "/latest.json") {
		archive := l.url.ResolveReference(&url.URL{Path: "revisions/" + c.Revision + ".json"})
		saved, e := l.read(ctx, archive)
		if e != nil {
			return c, e
		}
		if string(saved) != string(raw) {
			return c, fmt.Errorf("font catalogue immutable history mismatch")
		}
	} else if !strings.HasSuffix(l.url.Path, "/"+c.Revision+".json") {
		return c, fmt.Errorf("font catalogue revision mismatch")
	}
	l.cached = &c
	l.at = time.Now()
	return c, nil
}
