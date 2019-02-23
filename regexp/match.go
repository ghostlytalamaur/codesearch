// Copyright 2011 The Go Authors.  All rights reserved.
// Copyright 2013-2016 Manpreet Singh ( junkblocker@yahoo.com ). All rights reserved.
//
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package regexp

import (
	"bytes"
	"context"
	"encoding/binary"
	"flag"
	"fmt"
	"io"
	"os"
	"regexp/syntax"
	"sort"

	"github.com/ghostlytalamaur/codesearch/sparse"
	"github.com/logrusorgru/aurora"
)

// A matcher holds the state for running regular expression search.
type matcher struct {
	prog      *syntax.Prog       // compiled program
	dstate    map[string]*dstate // dstate cache
	start     *dstate            // start state
	startLine *dstate            // start state for beginning of line
	z1, z2    nstate             // two temporary nstates
}

// An nstate corresponds to an NFA state.
type nstate struct {
	q       sparse.Set // queue of program instructions
	partial rune       // partially decoded rune (TODO)
	flag    flags      // flags (TODO)
}

// The flags record state about a position between bytes in the text.
type flags uint32

const (
	flagBOL  flags = 1 << iota // beginning of line
	flagEOL                    // end of line
	flagBOT                    // beginning of text
	flagEOT                    // end of text
	flagWord                   // last byte was word byte
)

// A dstate corresponds to a DFA state.
type dstate struct {
	next     [256]*dstate // next state, per byte
	enc      string       // encoded nstate
	matchNL  bool         // match when next byte is \n
	matchEOT bool         // match in this state at end of text
}

func (z *nstate) String() string {
	return fmt.Sprintf("%v/%#x+%#x", z.q.Dense(), z.flag, z.partial)
}

// enc encodes z as a string.
func (z *nstate) enc() string {
	var buf []byte
	var v [10]byte
	last := ^uint32(0)
	n := binary.PutUvarint(v[:], uint64(z.partial))
	buf = append(buf, v[:n]...)
	n = binary.PutUvarint(v[:], uint64(z.flag))
	buf = append(buf, v[:n]...)
	dense := z.q.Dense()
	ids := make([]int, 0, len(dense))
	for _, id := range z.q.Dense() {
		ids = append(ids, int(id))
	}
	sort.Ints(ids)
	for _, id := range ids {
		n := binary.PutUvarint(v[:], uint64(uint32(id)-last))
		buf = append(buf, v[:n]...)
		last = uint32(id)
	}
	return string(buf)
}

// dec decodes the encoding s into z.
func (z *nstate) dec(s string) {
	b := []byte(s)
	i, n := binary.Uvarint(b)
	if n <= 0 {
		bug()
	}
	b = b[n:]
	z.partial = rune(i)
	i, n = binary.Uvarint(b)
	if n <= 0 {
		bug()
	}
	b = b[n:]
	z.flag = flags(i)
	z.q.Reset()
	last := ^uint32(0)
	for len(b) > 0 {
		i, n = binary.Uvarint(b)
		if n <= 0 {
			bug()
		}
		b = b[n:]
		last += uint32(i)
		z.q.Add(last)
	}
}

// dmatch is the state we're in when we've seen a match and are just
// waiting for the end of the line.
var dmatch = dstate{
	matchNL:  true,
	matchEOT: true,
}

func init() {
	var z nstate
	dmatch.enc = z.enc()
	for i := range dmatch.next {
		if i != '\n' {
			dmatch.next[i] = &dmatch
		}
	}
}

// init initializes the matcher.
func (m *matcher) init(prog *syntax.Prog) error {
	m.prog = prog
	m.dstate = make(map[string]*dstate)

	m.z1.q.Init(uint32(len(prog.Inst)))
	m.z2.q.Init(uint32(len(prog.Inst)))

	m.addq(&m.z1.q, uint32(prog.Start), syntax.EmptyBeginLine|syntax.EmptyBeginText)
	m.z1.flag = flagBOL | flagBOT
	m.start = m.cache(&m.z1)

	m.z1.q.Reset()
	m.addq(&m.z1.q, uint32(prog.Start), syntax.EmptyBeginLine)
	m.z1.flag = flagBOL
	m.startLine = m.cache(&m.z1)

	return nil
}

// stepEmpty steps runq to nextq expanding according to flag.
func (m *matcher) stepEmpty(runq, nextq *sparse.Set, flag syntax.EmptyOp) {
	nextq.Reset()
	for _, id := range runq.Dense() {
		m.addq(nextq, id, flag)
	}
}

// stepByte steps runq to nextq consuming c and then expanding according to flag.
// It returns true if a match ends immediately before c.
// c is either an input byte or endText.
func (m *matcher) stepByte(runq, nextq *sparse.Set, c int, flag syntax.EmptyOp) (match bool) {
	nextq.Reset()
	m.addq(nextq, uint32(m.prog.Start), flag)
	for _, id := range runq.Dense() {
		i := &m.prog.Inst[id]
		switch i.Op {
		default:
			continue
		case syntax.InstMatch:
			match = true
			continue
		case instByteRange:
			if c == endText {
				break
			}
			lo := int((i.Arg >> 8) & 0xFF)
			hi := int(i.Arg & 0xFF)
			ch := c
			if i.Arg&argFold != 0 && 'a' <= ch && ch <= 'z' {
				ch += 'A' - 'a'
			}
			if lo <= ch && ch <= hi {
				m.addq(nextq, i.Out, flag)
			}
		}
	}
	return
}

// addq adds id to the queue, expanding according to flag.
func (m *matcher) addq(q *sparse.Set, id uint32, flag syntax.EmptyOp) {
	if q.Has(id) {
		return
	}
	q.Add(id)
	i := &m.prog.Inst[id]
	switch i.Op {
	case syntax.InstCapture, syntax.InstNop:
		m.addq(q, i.Out, flag)
	case syntax.InstAlt, syntax.InstAltMatch:
		m.addq(q, i.Out, flag)
		m.addq(q, i.Arg, flag)
	case syntax.InstEmptyWidth:
		if syntax.EmptyOp(i.Arg)&^flag == 0 {
			m.addq(q, i.Out, flag)
		}
	}
}

const endText = -1

// computeNext computes the next DFA state if we're in d reading c (an input byte or endText).
func (m *matcher) computeNext(d *dstate, c int) *dstate {
	this, next := &m.z1, &m.z2
	this.dec(d.enc)

	// compute flags in effect before c
	flag := syntax.EmptyOp(0)
	if this.flag&flagBOL != 0 {
		flag |= syntax.EmptyBeginLine
	}
	if this.flag&flagBOT != 0 {
		flag |= syntax.EmptyBeginText
	}
	if this.flag&flagWord != 0 {
		if !isWordByte(c) {
			flag |= syntax.EmptyWordBoundary
		} else {
			flag |= syntax.EmptyNoWordBoundary
		}
	} else {
		if isWordByte(c) {
			flag |= syntax.EmptyWordBoundary
		} else {
			flag |= syntax.EmptyNoWordBoundary
		}
	}
	if c == '\n' {
		flag |= syntax.EmptyEndLine
	}
	if c == endText {
		flag |= syntax.EmptyEndLine | syntax.EmptyEndText
	}

	// re-expand queue using new flags.
	// TODO: only do this when it matters
	// (something is gating on word boundaries).
	m.stepEmpty(&this.q, &next.q, flag)
	this, next = next, this

	// now compute flags after c.
	flag = 0
	next.flag = 0
	if c == '\n' {
		flag |= syntax.EmptyBeginLine
		next.flag |= flagBOL
	}
	if isWordByte(c) {
		next.flag |= flagWord
	}

	// re-add start, process rune + expand according to flags.
	if m.stepByte(&this.q, &next.q, c, flag) {
		return &dmatch
	}
	return m.cache(next)
}

func (m *matcher) cache(z *nstate) *dstate {
	enc := z.enc()
	d := m.dstate[enc]
	if d != nil {
		return d
	}

	d = &dstate{enc: enc}
	m.dstate[enc] = d
	d.matchNL = m.computeNext(d, '\n') == &dmatch
	d.matchEOT = m.computeNext(d, endText) == &dmatch
	return d
}

func (m *matcher) match(b []byte, beginText, endText bool) (end int) {
	//	fmt.Printf("%v\n", m.prog)

	d := m.startLine
	if beginText {
		d = m.start
	}
	//	m.z1.dec(d.enc)
	//	fmt.Printf("%v (%v)\n", &m.z1, d==&dmatch)
	for i, c := range b {
		d1 := d.next[c]
		if d1 == nil {
			if c == '\n' {
				if d.matchNL {
					return i
				}
				//}
				d1 = m.startLine
			} else {
				d1 = m.computeNext(d, int(c))
			}
			d.next[c] = d1
		}
		d = d1
		//		m.z1.dec(d.enc)
		//		fmt.Printf("%#U: %v (%v, %v, %v)\n", c, &m.z1, d==&dmatch, d.matchNL, d.matchEOT)
	}
	if d.matchNL || endText && d.matchEOT {
		return len(b)
	}
	return -1
}

func (m *matcher) matchString(b string, beginText, endText bool) (end int) {
	d := m.startLine
	if beginText {
		d = m.start
	}
	for i := 0; i < len(b); i++ {
		c := b[i]
		d1 := d.next[c]
		if d1 == nil {
			if c == '\n' {
				if d.matchNL {
					return i
				}
				d1 = m.startLine
			} else {
				d1 = m.computeNext(d, int(c))
			}
			d.next[c] = d1
		}
		d = d1
	}
	if d.matchNL || endText && d.matchEOT {
		return len(b)
	}
	return -1
}

// isWordByte reports whether the byte c is a word character: ASCII only.
// This is used to implement \b and \B.  This is not right for Unicode, but:
//	- it's hard to get right in a byte-at-a-time matching world
//	  (the DFA has only one-byte lookahead)
//	- this crude approximation is the same one PCRE uses
func isWordByte(c int) bool {
	return 'A' <= c && c <= 'Z' ||
		'a' <= c && c <= 'z' ||
		'0' <= c && c <= '9' ||
		c == '_'
}

// Grep - TODO
type Grep struct {
	Regexp *Regexp   // regexp to search for
	Stdout io.Writer // output target
	Stderr io.Writer // error target
	Params GrepParams

	printer              *grepResultPrinter
	Done                 bool
	lines_printed        int64 // running match count
	max_print_lines      int64 // Max match count
	maxPrintLinesPerFile int64 // Max match count

	Match bool

	buf []byte
}

type GrepResult struct {
	FileName string
	LineNum  int
	Text     string
}

// GrepParams params for Grep
type GrepParams struct {
	L             bool // L flag - print file names only
	Z             bool // 0 flag - print matches separated by \0
	C             bool // C flag - print count of matches
	N             bool // N flag - print line numbers
	H             bool // H flag - do not print file names
	useRe2        bool // Use Go regexp engine
	addLinesCount uint
	PrinterParams PrinterParams
}

// AddFlags : Add flags to command line parser
func (p *GrepParams) AddFlags() {
	flag.BoolVar(&p.useRe2, "re2", false, "Use Go regexp engine")
	p.PrinterParams.addFlags()
}

// File - start search in file
func (g *Grep) File(ctx context.Context, name string, output chan<- GrepResult) bool {
	f, err := os.Open(name)
	if err != nil {
		fmt.Fprintf(g.Stderr, "%s\n", err)
		return false
	}
	defer f.Close()
	if g.printer == nil {
		g.printer = &grepResultPrinter{
			Output: g.Stdout,
			Params: g.Params.PrinterParams,
		}
	}
	return g.UniReader2(ctx, f, name, g.printer, output)
}

func (g *Grep) LimitPrintCount(globalLimit int64, fileLimit int64) {
	g.Done = false
	g.lines_printed = 0
	g.max_print_lines = globalLimit
	if g.max_print_lines < 0 {
		g.max_print_lines = 0
	}
	g.maxPrintLinesPerFile = fileLimit
	if g.maxPrintLinesPerFile < 0 {
		g.maxPrintLinesPerFile = 0
	}
	g.Params.PrinterParams.PrintedLinesLimit = fileLimit
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
			} else {
				skipLines--
			}
		}
	}
	if isDirect {
		return 0
	} else {
		return len(buf)
	}
}

func (g *Grep) UniReader(r io.Reader, name string) {
	if g.Done {
		return
	}
	if g.buf == nil {
		g.buf = make([]byte, 1<<20)
	}
	var (
		buf                  = g.buf[:0]
		needLineno           = g.Params.N
		lineno               = 1
		count                = 0
		prefix               = ""
		beginText            = true
		endText              = false
		outSep               = '\n'
		printedForFile int64 = 0
	)
	if !g.Params.H {
		prefix = name + ":"
	}
	if g.Params.L && g.Params.Z {
		outSep = '\x00'
	}
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
			var matchStart, matchEnd int
			if g.Params.useRe2 {
				matchStart, matchEnd = g.Regexp.MatchRe2(buf[chunkStart:end], beginText, endText)
			} else {
				matchStart, matchEnd = g.Regexp.MatchDef(buf[chunkStart:end], beginText, endText)
			}
			matchStart += chunkStart
			matchEnd += chunkStart
			if matchStart > matchEnd {
				break
			}
			beginText = false
			//if m1 < chunkStart {
			//	break
			//}
			g.Match = true
			if g.Params.L {
				fmt.Fprintf(g.Stdout, "%s%c", name, outSep)
				g.lines_printed++
				if g.max_print_lines > 0 && g.lines_printed >= g.max_print_lines {
					g.Done = true
				}
				return
			}
			lineStart := bytes.LastIndex(buf[chunkStart:matchStart], nl) + 1 + chunkStart
			lineEnd := len(buf)
			if nlIndex := bytes.Index(buf[matchEnd:], nl); nlIndex >= 0 {
				lineEnd = nlIndex + 1 + matchEnd
			}
			//lineEnd := matchEnd + 1
			if lineEnd > end {
				lineEnd = end
			}
			if needLineno {
				lineno += countNL(buf[chunkStart:lineStart])
			}
			//line := buf[lineStart:lineEnd]
			preMatchStr := buf[lineStart:matchStart]
			matchStr := buf[matchStart:matchEnd]
			postMatchStr := buf[matchEnd:lineEnd]
			if g.Params.addLinesCount > 0 {
				idx := findNL(buf, lineStart, false, g.Params.addLinesCount)
				if idx != 0 {
					idx++
				}
				preMatchStr = buf[idx:matchStart]
				idx = findNL(buf, matchEnd, true, g.Params.addLinesCount)
				postMatchStr = buf[matchEnd : idx+1]
			}
			switch {
			case g.Params.C:
				count++
			case g.Params.N:
				fmt.Fprintf(g.Stdout, "%s%d:%s%s%s", prefix, lineno, preMatchStr, aurora.Bold(matchStr), postMatchStr)
				g.lines_printed++
				printedForFile++
				if g.max_print_lines > 0 && g.lines_printed >= g.max_print_lines {
					g.Done = true
					return
				}
				if g.maxPrintLinesPerFile > 0 && printedForFile >= g.maxPrintLinesPerFile {
					return
				}
			default:
				fmt.Fprintf(g.Stdout, "%s%s%s%s", prefix, preMatchStr, aurora.Bold(matchStr), postMatchStr)
				g.lines_printed++
				printedForFile++
				if g.max_print_lines > 0 && g.lines_printed >= g.max_print_lines {
					g.Done = true
					return
				}
				if g.maxPrintLinesPerFile > 0 && printedForFile >= g.maxPrintLinesPerFile {
					return
				}
			}
			if needLineno {
				lineno++
			}
			chunkStart = lineEnd
		}
		if needLineno && err == nil {
			lineno += countNL(buf[chunkStart:end])
		}
		n = copy(buf, buf[end:])
		buf = buf[:n]
		if len(buf) == 0 && err != nil {
			if err != io.EOF && err != io.ErrUnexpectedEOF {
				fmt.Fprintf(g.Stderr, "%s: %v\n", name, err)
				// error lines do not count towards max lines printed
			}
			break
		}
	}
	if g.Params.C && count > 0 {
		fmt.Fprintf(g.Stdout, "%s: %d\n", name, count)
		g.lines_printed++
		if g.max_print_lines > 0 && g.lines_printed >= g.max_print_lines {
			g.Done = true
			return
		}
	}
}

type grepResultPrinter struct {
	Output io.Writer
	Params PrinterParams

	// cached variables
	lineNum      int
	matchesCount int
	fileName     string
	linesPrinted int64
}

type PrinterParams struct {
	PrintFileNamesOnly         bool // L flag - print file names only
	PrintNullTerminatedStrings bool // 0 flag - print matches separated by \0
	PrintMatchesCount          bool // C flag - print count of matches
	PrintLineNumbers           bool // N flag - print line numbers
	PrintFileName              bool // H flag - do not print file names
	AddLinesCount              uint
	PrintedLinesLimit          int64
}

func (p *PrinterParams) addFlags() {
	flag.BoolVar(&p.PrintFileNamesOnly, "l", false, "list matching files only")
	flag.BoolVar(&p.PrintNullTerminatedStrings, "0", false, "list filename matches separated by NUL ('\\0') character. Requires -l option")
	flag.BoolVar(&p.PrintMatchesCount, "c", false, "print match counts only")
	flag.BoolVar(&p.PrintLineNumbers, "n", false, "show line numbers")
	flag.BoolVar(&p.PrintFileName, "h", false, "omit file names")
	flag.UintVar(&p.AddLinesCount, "addlines", 0, "print additional lines")
}

func (p *grepResultPrinter) makeResult(buffer []byte, start int, end int) GrepResult {
	var text string
	if p.Params.PrintFileNamesOnly {
		if p.Params.PrintNullTerminatedStrings {
			text = fmt.Sprintf("%s%s", p.fileName, "\x00")
		} else {
			text = fmt.Sprintf("%s%s", p.fileName, "\n")
		}
	} else {

		lineStart := bytes.LastIndex(buffer[:start], nl) + 1
		lineEnd := len(buffer)

		if nlIndex := bytes.Index(buffer[end:], nl); nlIndex >= 0 {
			lineEnd = nlIndex + 1 + end
		}
		if lineEnd > len(buffer) {
			lineEnd = len(buffer)

		}

		preMatchStr := buffer[lineStart:start]
		matchStr := buffer[start:end]
		postMatchStr := buffer[end:lineEnd]

		if p.Params.AddLinesCount > 0 {
			idx := findNL(buffer, lineStart, false, p.Params.AddLinesCount)
			if idx != 0 {
				idx++
			}
			preMatchStr = buffer[idx:start]
			idx = findNL(buffer, end, true, p.Params.AddLinesCount)
			postMatchStr = buffer[end : idx+1]
		}
		text = fmt.Sprintf("%s%s%s", preMatchStr, aurora.Bold(matchStr), postMatchStr)
	}

	return GrepResult{
		FileName: p.fileName,
		LineNum:  p.lineNum,
		Text:     text,
	}
}

func (p *grepResultPrinter) countLineNums(buffer []byte) {
	if p.Params.PrintLineNumbers {
		p.lineNum += countNL(buffer)
	}
}

func (p *grepResultPrinter) startSearch(fileName string) {
	p.lineNum = 1
	p.matchesCount = 0
	p.linesPrinted = 0
	p.fileName = fileName
}

func (p *grepResultPrinter) endSearch() {
	if p.Params.PrintMatchesCount && p.matchesCount > 0 {
		fmt.Fprintf(p.Output, "%s: %d\n", p.fileName, p.matchesCount)
		p.linesPrinted++
	}
}

func (g *Grep) match(buf []byte, beginText bool, endText bool) (a, b int) {
	if g.Params.useRe2 {
		return g.Regexp.MatchRe2(buf, beginText, endText)
	} else {
		return g.Regexp.MatchDef(buf, beginText, endText)
	}
}

func (g *Grep) isDone(ctx context.Context, format string, a ...interface{}) bool {
	select {
	case <-ctx.Done():
		fmt.Fprintf(g.Stderr, format, a)
		return true
	default:
		return false
	}
}

func inc(a int, b int, delta int) (int, int) {
	return a + delta, b + delta
}

func (g *Grep) UniReader2(ctx context.Context, r io.Reader, name string, printer *grepResultPrinter,
	output chan<- GrepResult) (match bool) {
	match = false
	if g.buf == nil {
		g.buf = make([]byte, 1<<20)
	}
	var (
		buf       = g.buf[:0]
		beginText = true
		endText   = false
	)
	printer.startSearch(name)
	defer printer.endSearch()
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
				return match
			}
			matchStart, matchEnd := g.match(buf[chunkStart:end], beginText, endText)
			if matchStart > matchEnd {
				break
			}

			matchStart, matchEnd = matchStart+chunkStart, matchEnd+chunkStart
			if g.isDone(ctx, "Search cancelled. File: %s\n", name) {
				return match
			}

			match = true
			printer.countLineNums(buf[chunkStart:matchStart])
			select {
			case <-ctx.Done():
				return match
			case output <- printer.makeResult(buf, matchStart, matchEnd):
			}
			if nlIndex := bytes.Index(buf[matchEnd:], nl); nlIndex >= 0 {
				chunkStart = nlIndex + 1 + matchEnd
			} else {
				chunkStart = end
			}
			if chunkStart > end {
				chunkStart = end
			}
		}
		if err == nil {
			printer.countLineNums(buf[chunkStart:end])
		}
		n = copy(buf, buf[end:])
		buf = buf[:n]
		if len(buf) == 0 && err != nil {
			if err != io.EOF && err != io.ErrUnexpectedEOF {
				fmt.Fprintf(g.Stderr, "%s: %v\n", name, err)
				// error lines do not count towards max lines printed
			}
			break
		}
	}

	return match
}
