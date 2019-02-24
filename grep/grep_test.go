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
	re string
	in string
	n  int
	s  string
}{
	{re: `abc`, in: "abc\nbca", n: 1, s: "abc\n"},
	{re: `bca`, in: "abc\nbca", n: 2, s: "bca"},
	{re: `bca`, in: "abc\nbca\n", n: 2, s: "bca\n"},
}

func TestGrepResult(t *testing.T) {
	for i, tt := range grepResultTest {
		re, err := regexp.Compile("(?m)" + tt.re)
		if err != nil {
			t.Errorf("Compile(%#q): %v", tt.re, err)
			continue
		}
		var g Grep
		g.Params.disableColors = true
		g.Regexp = re
		for r := range g.Reader(context.Background(), "input", strings.NewReader(tt.in)) {
			if r.LineNum != tt.n || r.Text != tt.s {
				t.Errorf("#%d: grep(%#q, %q) = (%d, %q), want (%d, %q)", i, tt.re, tt.in, r.LineNum, r.Text, tt.n, tt.s)
			}
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
		g := tt.g
		g.Params.disableColors = true
		g.Regexp = re
		var out, errb bytes.Buffer
		log.SetOutput(&errb)
		log.SetLevel(log.ErrorLevel)
		for r := range g.Reader(context.Background(), "input", strings.NewReader(tt.s)) {
			fmt.Fprintf(&out, "%s:%s", r.FileName, r.Text)
		}
		println(out.String())
		if out.String() != tt.out || errb.String() != tt.err {
			t.Errorf("#%d: grep(%#q, %q) = %q, %q, want %q, %q", i, tt.re, tt.s, out.String(), errb.String(), tt.out, tt.err)
		}
	}
}
