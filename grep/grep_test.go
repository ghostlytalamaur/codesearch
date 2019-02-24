package grep

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/ghostlytalamaur/codesearch/regexp"
	log "github.com/sirupsen/logrus"
)

var grepTests = []struct {
	re  string
	s   string
	out string
	err string
	g   Grep
}{
	{re: `a+`, s: "abc\ndef\nghalloo\n", out: "input:abc\ninput:ghalloo\n"},
	{re: `x.*y`, s: "xay\nxa\ny\n", out: "input:xay\n"},
	{re: `var.*`, s: "var\r\n  I: Integer;", out: "input:var\r\n"},
	// {re: `(?s)(?m)var.*Integer`, s: "var\r\n  I: Integer;\r\n", out: "input:var\r\n  I: Integer;\r\n"},
}

var grepResultTest = []struct {
	isRe2 bool
	re    string
	in    string
	n     int
	s     string
	cnt   int
}{
	{isRe2: false, re: `abc`, in: "abc\nbca", n: 1, s: "abc\n", cnt: 1},
	{isRe2: false, re: `bca`, in: "abc\nbca", n: 2, s: "bca", cnt: 1},
	{isRe2: false, re: `bca`, in: "abc\nbca\n", n: 2, s: "bca\n", cnt: 1},
	{isRe2: false, re: `bca`, in: "abc\nbca\n", n: 2, s: "bca\n", cnt: 1},
	{isRe2: true, re: `(?s)c.*bca`, in: "ab\nc\nbca\n", n: 2, s: "c\nbca\n", cnt: 1},
}

func TestGrepResult(t *testing.T) {
	for i, tt := range grepResultTest {
		re, err := regexp.Compile("(?m)" + tt.re)
		if err != nil {
			t.Errorf("Compile(%#q): %v", tt.re, err)
			continue
		}
		var g Grep
		var errb bytes.Buffer
		log.SetOutput(&errb)
		log.SetLevel(log.ErrorLevel)

		formatParams := ResultFormatParams{
			WithColors:           false,
			PrintLineNumbers:     false,
			PrintWithoutFileName: true,
		}
		g.Params.disableColors = true
		g.Regexp = re
		g.Params.useRe2 = tt.isRe2
		var matches int

		if errb.String() != "" {
			t.Errorf("#%d: grep(%#q, %q) has errors %s", i, tt.re, tt.in, errb.String())
		}
		for r := range g.Reader(context.Background(), "input", strings.NewReader(tt.in)) {
			matches++
			text := string(r.Format(&formatParams))
			if r.LineNum != tt.n || text != tt.s {
				t.Errorf("#%d: grep(%#q, %q) = (%d, %q), want (%d, %q)", i, tt.re, tt.in, r.LineNum, text, tt.n, tt.s)
			}
		}
		if matches != tt.cnt {
			t.Errorf("#%d: grep(%#q, %q) incorrect matches count, expected %d, but was %d", i, tt.re, tt.in, tt.cnt, matches)
		}
	}
}

func TestGrep(t *testing.T) {
	for i, tt := range grepTests {
		re, err := regexp.Compile("(?m)" + tt.re)
		if err != nil {
			t.Errorf("Compile(%#q): %v", tt.re, err)
			continue
		}
		formatParams := ResultFormatParams{
			WithColors:           false,
			PrintLineNumbers:     false,
			PrintWithoutFileName: true,
		}
		g := tt.g
		g.Params.disableColors = true
		g.Regexp = re
		var out, errb bytes.Buffer
		log.SetOutput(&errb)
		log.SetLevel(log.ErrorLevel)
		for r := range g.Reader(context.Background(), "input", strings.NewReader(tt.s)) {
			text := string(r.Format(&formatParams))
			fmt.Fprintf(&out, "%s:%s", r.FileName, text)
		}
		println(out.String())
		if out.String() != tt.out || errb.String() != tt.err {
			t.Errorf("#%d: grep(%#q, %q) = %q, %q, want %q, %q", i, tt.re, tt.s, out.String(), errb.String(), tt.out, tt.err)
		}
	}
}
