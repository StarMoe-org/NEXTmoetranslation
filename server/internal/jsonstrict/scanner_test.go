package jsonstrict

import (
	"fmt"
	"strings"
	"testing"
)

func TestScanValidAndEdgeRejections(t *testing.T) {
	valid := `{"a":1,"b":[true,false,-0.25e+3,"hello\n\uD83D\uDE00"],"c":{}}`
	if err := Scan([]byte(valid), Options{
		MaxDepth:             8,
		ForbidNull:           true,
		ForbidLoneSurrogates: true,
		RequireValidUTF8:     true,
	}); err != nil {
		t.Fatalf("unexpected error on valid JSON: %v", err)
	}

	validBytes := []byte(valid)
	allocs := testing.AllocsPerRun(100, func() {
		_ = Scan(validBytes, Options{
			MaxDepth:             8,
			ForbidNull:           true,
			ForbidLoneSurrogates: true,
		})
	})
	if allocs != 0 {
		t.Fatalf("Scan allocated %.1f objects per run, want 0", allocs)
	}

	rejections := map[string]struct {
		raw  string
		opts Options
	}{
		"empty":                  {"   ", Options{}},
		"trailing value":         {`{"a":1} {}`, Options{}},
		"duplicate inline key":   {`{"a":1,"b":2,"a":3}`, Options{}},
		"duplicate escaped key":  {`{"version":3,"\u0076ersion":3}`, Options{}},
		"trailing object comma":  {`{"a":1,}`, Options{}},
		"trailing array comma":   {`[1,2,]`, Options{}},
		"leading zero number":    {`{"a":01}`, Options{}},
		"null forbidden":         {`{"a":[1,null]}`, Options{ForbidNull: true}},
		"lone high surrogate":    {`{"a":"\uD800"}`, Options{ForbidLoneSurrogates: true}},
		"lone low surrogate":     {`{"a":"\uDC00"}`, Options{ForbidLoneSurrogates: true}},
		"depth exceeded":         {`[[[1]]]`, Options{MaxDepth: 3}},
		"raw control char":       {"{\"a\":\"\x01\"}", Options{}},
		"invalid utf8":           {"{\"a\":\"\xff\"}", Options{RequireValidUTF8: true}},
	}
	for name, tc := range rejections {
		t.Run(name, func(t *testing.T) {
			if err := Scan([]byte(tc.raw), tc.opts); err == nil {
				t.Fatalf("expected error for %s (%q), got nil", name, tc.raw)
			}
		})
	}

	// Verify large object (>24 keys) duplicate detection promotes cleanly.
	var sb strings.Builder
	sb.WriteByte('{')
	for i := 0; i < 40; i++ {
		if i > 0 {
			sb.WriteByte(',')
		}
		fmt.Fprintf(&sb, `"k%d":%d`, i, i)
	}
	base := sb.String()
	if err := Scan([]byte(base+"}"), Options{}); err != nil {
		t.Fatalf("40-key unique object rejected: %v", err)
	}
	if err := Scan([]byte(base+`,"k5":99}`), Options{}); err == nil {
		t.Fatal("40-key object with duplicate key was accepted")
	}
}
