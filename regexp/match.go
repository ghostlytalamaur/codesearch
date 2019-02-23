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

	log "github.com/sirupsen/logrus"

	"github.com/ghostlytalamaur/codesearch/sparse"
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
	Regexp *Regexp // regexp to search for
	Params GrepParams

	buf []byte
}

// GrepResult represend one match
type GrepResult struct {
	FileName string
	LineNum  int
	Text     string
}

// GrepParams params for Grep
type GrepParams struct {
	useRe2        bool // Use Go regexp engine
	addLinesCount uint
}

// AddFlags : Add flags to command line parser
func (p *GrepParams) AddFlags() {
	flag.BoolVar(&p.useRe2, "re2", false, "Use Go regexp engine")
	flag.UintVar(&p.addLinesCount, "addlines", 0, "print additional lines")
}

// File - start search in file
func (g *Grep) File(ctx context.Context, name string) <-chan GrepResult {
	out := make(chan GrepResult)
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

func (g *Grep) Reader(ctx context.Context, name string, reader io.Reader) <-chan GrepResult {
	out := make(chan GrepResult)
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
	params   *GrepParams
	fileName string
	lineNum  int
}

func (p *grepResultMaker) makeResult(buffer []byte, chunkStart, start int, end int) (res GrepResult, lineEnd int) {
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
	// aurora.Bold(matchStr)
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
	text = fmt.Sprintf("%s%s%s", preMatchStr, matchStr, postMatchStr)
	return GrepResult{
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

func (g *Grep) uniReader2(ctx context.Context, r io.Reader, name string, output chan<- GrepResult) {
	if g.buf == nil {
		g.buf = make([]byte, 1<<20)
	}

	maker := grepResultMaker{
		params:   &g.Params,
		fileName: name,
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
