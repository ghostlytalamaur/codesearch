// Copyright 2011 The Go Authors.  All rights reserved.
// Copyright 2013 Manpreet Singh ( junkblocker@yahoo.com ). All rights reserved.
//
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package regexp implements regular expression search tuned for
// use in grep-like programs.
package regexp

import (
	"regexp"
	"regexp/syntax"
)

func bug() {
	panic("codesearch/regexp: internal error")
}

// Regexp is the representation of a compiled regular expression.
// A Regexp is NOT SAFE for concurrent use by multiple goroutines.
type Regexp struct {
	Syntax *syntax.Regexp
	expr   string // original expression
	m      matcher
	re2    *regexp.Regexp
}

// String returns the source text used to compile the regular expression.
func (re *Regexp) String() string {
	return re.expr
}

// Compile parses a regular expression and returns, if successful,
// a Regexp object that can be used to match against lines of text.
func Compile(expr string) (*Regexp, error) {
	re, err := syntax.Parse(expr, syntax.Perl)
	if err != nil {
		return nil, err
	}
	sre := re.Simplify()
	prog, err := syntax.Compile(sre)
	if err != nil {
		return nil, err
	}
	if err := toByteProg(prog); err != nil {
		return nil, err
	}
	r := &Regexp{
		Syntax: re,
		expr:   expr,
	}
	if err := r.m.init(prog); err != nil {
		return nil, err
	}
	re2, err := regexp.Compile(expr)
	if err != nil {
		return nil, err
	}
	r.re2 = re2
	return r, nil
}

func (r *Regexp) Match(b []byte, beginText, endText bool) (end int) {
	return r.m.match(b, beginText, endText)
}

func (r *Regexp) Match2(b []byte, beginText, endText bool) (start, end int) {
	re2match := r.re2.FindSubmatchIndex(b)
	start = 1
	end = -1
	if re2match != nil {
		start = re2match[0]
		end = re2match[1]
		//for i := re2match[1]; i < len(b); i++ {
		//	if b[i] == '\n' {
		//		end = i
		//		break
		//	}
		//}
	}

	//m := r.m.match(b, beginText, endText)
	//fmt.Printf("Buffer:\n%s\n\n", b)
	//fmt.Printf("Re2: %v, end = %d\n", re2match, end)
	//fmt.Println("Matcher: ", m)
	return start, end
}

func (r *Regexp) MatchString(s string, beginText, endText bool) (end int) {
	return r.m.matchString(s, beginText, endText)
}

func (r *Regexp) MatchString2(s string, beginText, endText bool) (start, end int) {
	re2match := r.re2.FindStringSubmatchIndex(s)
	start = 1
	end = -1
	if re2match != nil {
		start = re2match[0]
		end = re2match[1]
		//for i := re2match[1]; i < len(b); i++ {
		//	if b[i] == '\n' {
		//		end = i
		//		break
		//	}
		//}
	}

	//m := r.m.match(b, beginText, endText)
	//fmt.Printf("Buffer:\n%s\n\n", b)
	//fmt.Printf("Re2: %v, end = %d\n", re2match, end)
	//fmt.Println("Matcher: ", m)
	return start, end
}
