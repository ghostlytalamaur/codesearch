package grep

import (
	"bytes"
	"context"
	"flag"
	"io"
	"os"
	"strconv"

	"github.com/ghostlytalamaur/codesearch/regexp"
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
	FileName   string
	LineNum    int
	textBuf    []byte
	matchStart int
	matchEnd   int
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
	PrintFileNamesOnly         bool // print file names only
	PrintNullTerminatedStrings bool // print matches separated by \0
	PrintLineNumbers           bool // print line numbers
	PrintWithoutFileName       bool // do not print file names
	WithColors                 bool // highlight match with ansi escape bold code
	EnsureNewLine              bool // ensure that text contains new line at the end
}

// AddFlags add command line parser flags
func (p *ResultFormatParams) AddFlags() {
	flag.BoolVar(&p.PrintFileNamesOnly, "l", false, "list matching files only")
	flag.BoolVar(&p.PrintNullTerminatedStrings, "0", false, "list filename matches separated by NUL ('\\0') character. Requires -l option")
	flag.BoolVar(&p.PrintLineNumbers, "n", false, "show line numbers")
	flag.BoolVar(&p.PrintWithoutFileName, "h", false, "omit file names")
}

var (
	boldStart = []byte{'\033', '[', '1', 'm'}
	boldReset = []byte{'\033', '[', '0', 'm'}
)

// Format convert GrepResult to string using parameters
func (r *Result) Format(params *ResultFormatParams) []byte {
	var b bytes.Buffer
	if params.PrintFileNamesOnly {
		b.WriteString(r.FileName)
		if params.PrintNullTerminatedStrings {
			b.WriteByte('\x00')
		} else {
			b.WriteByte('\n')
		}
	} else {
		if !params.PrintWithoutFileName {
			b.WriteString(r.FileName)
			b.WriteByte(':')
		}
		if params.PrintLineNumbers {
			b.WriteString(strconv.Itoa(r.LineNum))
			b.WriteByte(':')
		}
		if params.WithColors {
			b.Write(r.textBuf[:r.matchStart])
			b.Write(boldStart)
			b.Write(r.textBuf[r.matchStart:r.matchEnd])
			b.Write(boldReset)
			b.Write(r.textBuf[r.matchEnd:])
		} else {
			b.Write(r.textBuf)
		}
	}
	if b.Len() > 0 && params.EnsureNewLine && b.Bytes()[len(b.Bytes())-1] != '\n' {
		b.WriteByte('\n')
	}
	return b.Bytes()
}

// File - start search in file
func (g *Grep) File(ctx context.Context, name string) <-chan Result {
	out := make(chan Result, 1)
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
	out := make(chan Result, 1)
	go func() {
		defer close(out)
		g.uniReader2(ctx, reader, name, out)
	}()
	return out
}

var nl = []byte{'\n'}

func findNL(buf []byte, startpos int, isDirect bool, skipLines uint) int {
	direction := 1
	if !isDirect {
		direction = -1
	}

	for i := startpos; 0 <= i && i < len(buf); i += direction {
		if buf[i] == '\n' {
			if skipLines == 0 {
				return i
			}
			skipLines--
		}
	}
	if isDirect {
		return len(buf)
	}
	return 0
}

type grepResultMaker struct {
	params   *Params
	fileName string
	lineNum  int
}

func (p *grepResultMaker) makeResult(buffer []byte, chunkStart, start int, end int) (res Result, lineEnd int) {
	p.lineNum += bytes.Count(buffer[chunkStart:start], nl)
	var lineStart int
	if p.params.addLinesCount > 0 {
		lineStart = findNL(buffer, start, false, p.params.addLinesCount)
		if lineStart != 0 {
			lineStart++
		}
		lineEnd = findNL(buffer, end, true, p.params.addLinesCount) + 1
	} else {
		lineStart = bytes.LastIndex(buffer[:start], nl) + 1
		lineEnd = len(buffer)

		if nlIndex := bytes.Index(buffer[end:], nl); nlIndex >= 0 {
			lineEnd = nlIndex + 1 + end
		}
	}

	if lineEnd > len(buffer) {
		lineEnd = len(buffer)
	}

	b := make([]byte, lineEnd-lineStart)
	copy(b, buffer[lineStart:lineEnd])
	return Result{
		FileName:   p.fileName,
		LineNum:    p.lineNum,
		matchStart: start - lineStart,
		matchEnd:   end - lineStart,
		textBuf:    b,
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
			maker.lineNum += bytes.Count(buf[matchEnd:lineEnd], nl)
			chunkStart = lineEnd
			if chunkStart > end {
				chunkStart = end
			}
		}
		if err == nil {
			maker.lineNum += bytes.Count(buf[chunkStart:end], nl)
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
