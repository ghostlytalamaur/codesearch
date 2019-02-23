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

func TestGrep(t *testing.T) {
	for i, tt := range grepTests {
		re, err := regexp.Compile("(?m)" + tt.re)
		if err != nil {
			t.Errorf("Compile(%#q): %v", tt.re, err)
			continue
		}
		g := tt.g
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
