package grep

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/ghostlytalamaur/codesearch/regexp"
	"github.com/logrusorgru/aurora"
	log "github.com/sirupsen/logrus"
)

// Grep - TODO
type Grep struct {
	Regexp *regexp.Regexp // regexp to search for
	Params Params

	buf []byte
}

// Result represend one match
type Result struct {
	FileName string
	LineNum  int
	Text     string
}

// Params params for Grep
type Params struct {
	useRe2        bool // Use Go regexp engine
	addLinesCount uint
	disableColors bool
}

// AddFlags : Add flags to command line parser
func (p *Params) AddFlags() {
	flag.BoolVar(&p.useRe2, "re2", false, "Use Go regexp engine")
	flag.UintVar(&p.addLinesCount, "addlines", 0, "print additional lines")
}

// ResultFormatParams parameters for formatting GrepResult
type ResultFormatParams struct {
	PrintFileNamesOnly         bool // L flag - print file names only
	PrintNullTerminatedStrings bool // 0 flag - print matches separated by \0
	PrintLineNumbers           bool // N flag - print line numbers
	PrintWithoutFileName       bool // H flag - do not print file names
}

// AddFlags add command line parser flags
func (p *ResultFormatParams) AddFlags() {
	flag.BoolVar(&p.PrintFileNamesOnly, "l", false, "list matching files only")
	flag.BoolVar(&p.PrintNullTerminatedStrings, "0", false, "list filename matches separated by NUL ('\\0') character. Requires -l option")
	flag.BoolVar(&p.PrintLineNumbers, "n", false, "show line numbers")
	flag.BoolVar(&p.PrintWithoutFileName, "h", false, "omit file names")
}

// Format convert GrepResult to string using parameters
func (r *Result) Format(params *ResultFormatParams) string {
	var text string
	if params.PrintFileNamesOnly {
		if params.PrintNullTerminatedStrings {
			text = fmt.Sprintf("%s\x00", r.FileName)
		} else {
			text = fmt.Sprintf("%s\n", r.FileName)
		}
	} else {
		if !params.PrintWithoutFileName {
			text = r.FileName + ":"
		}
		if params.PrintLineNumbers {
			text = fmt.Sprintf("%s%d:", text, r.LineNum)
		}
		text = fmt.Sprintf("%s%s", text, r.Text)
	}
	return text
}

// File - start search in file
func (g *Grep) File(ctx context.Context, name string) <-chan Result {
	out := make(chan Result)
	go func() {
		defer close(out)
		f, err := os.Open(name)
		if err != nil {
			log.Errorf("%s\n", err)
			return
		}
		defer f.Close()
		g.uniReader2(ctx, f, name, out)
	}()

	return out
}

// Reader run search on given reader.
// Context may be used for cancellation.
// Name used as FileName in GrepResult
func (g *Grep) Reader(ctx context.Context, name string, reader io.Reader) <-chan Result {
	out := make(chan Result)
	go func() {
		defer close(out)
		g.uniReader2(ctx, reader, name, out)
	}()
	return out
}

var nl = []byte{'\n'}

func countNL(b []byte) int {
	n := 0
	for {
		i := bytes.IndexByte(b, '\n')
		if i < 0 {
			break
		}
		n++
		b = b[i+1:]
	}
	return n
}

func findNL(buf []byte, startpos int, isDirect bool, skipLines uint) int {
	direction := 1
	if !isDirect {
		direction = -1
	}

	for i := startpos; 0 <= i && i <= len(buf); i += direction {
		ch := buf[i]
		if ch == '\n' {
			if skipLines == 0 {
				return i
			}
			skipLines--
		}
	}
	if isDirect {
		return 0
	}
	return len(buf)
}

type grepResultMaker struct {
	params   *Params
	fileName string
	lineNum  int
}

func (p *grepResultMaker) makeResult(buffer []byte, chunkStart, start int, end int) (res Result, lineEnd int) {
	p.lineNum += countNL(buffer[chunkStart:start])
	var text string
	lineStart := bytes.LastIndex(buffer[:start], nl) + 1
	lineEnd = len(buffer)

	if nlIndex := bytes.Index(buffer[end:], nl); nlIndex >= 0 {
		lineEnd = nlIndex + 1 + end
	}
	if lineEnd > len(buffer) {
		lineEnd = len(buffer)
	}

	preMatchStr := buffer[lineStart:start]
	matchStr := buffer[start:end]
	postMatchStr := buffer[end:lineEnd]

	if p.params.addLinesCount > 0 {
		idx := findNL(buffer, lineStart, false, p.params.addLinesCount)
		if idx != 0 {
			idx++
		}
		preMatchStr = buffer[idx:start]
		idx = findNL(buffer, end, true, p.params.addLinesCount)
		postMatchStr = buffer[end : idx+1]
	}
	if p.params.disableColors {
		text = fmt.Sprintf("%s%s%s", preMatchStr, matchStr, postMatchStr)
	} else {
		text = fmt.Sprintf("%s%s%s", preMatchStr, aurora.Bold(matchStr), postMatchStr)
	}
	return Result{
		FileName: p.fileName,
		LineNum:  p.lineNum,
		Text:     text,
	}, lineEnd
}

func (g *Grep) match(buf []byte, beginText bool, endText bool) (a, b int) {
	if g.Params.useRe2 {
		return g.Regexp.MatchRe2(buf, beginText, endText)
	}
	return g.Regexp.MatchDef(buf, beginText, endText)
}

func (g *Grep) isDone(ctx context.Context, format string, a ...interface{}) bool {
	select {
	case <-ctx.Done():
		log.Tracef(format, a)
		return true
	default:
		return false
	}
}

func (g *Grep) uniReader2(ctx context.Context, r io.Reader, name string, output chan<- Result) {
	if g.buf == nil {
		g.buf = make([]byte, 1<<20)
	}

	maker := grepResultMaker{
		params:   &g.Params,
		fileName: name,
		lineNum:  1,
	}

	var (
		buf       = g.buf[:0]
		beginText = true
		endText   = false
	)
	for {
		n, err := io.ReadFull(r, buf[len(buf):cap(buf)])
		buf = buf[:len(buf)+n]
		end := len(buf)
		if err == nil {
			end = bytes.LastIndex(buf, nl) + 1
		} else {
			endText = true
		}
		chunkStart := 0
		for chunkStart < end {
			if g.isDone(ctx, "Search cancelled. File: %s\n", name) {
				return
			}
			matchStart, matchEnd := g.match(buf[chunkStart:end], beginText, endText)
			if matchStart > matchEnd {
				break
			}

			matchStart, matchEnd = matchStart+chunkStart, matchEnd+chunkStart
			if g.isDone(ctx, "Search cancelled. File: %s\n", name) {
				return
			}

			r, lineEnd := maker.makeResult(buf, chunkStart, matchStart, matchEnd)
			select {
			case <-ctx.Done():
				return
			case output <- r:
			}
			chunkStart = lineEnd
			if chunkStart > end {
				chunkStart = end
			}
		}
		if err == nil {
			maker.lineNum += countNL(buf[chunkStart:end])
		}
		n = copy(buf, buf[end:])
		buf = buf[:n]
		if len(buf) == 0 && err != nil {
			if err != io.EOF && err != io.ErrUnexpectedEOF {
				log.Errorf("%s: %v\n", name, err)
			}
			break
		}
	}
}
