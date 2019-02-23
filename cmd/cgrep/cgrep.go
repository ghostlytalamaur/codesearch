// Copyright 2011 The Go Authors.  All rights reserved.
// Copyright 2013 Manpreet Singh ( junkblocker@yahoo.com ). All rights reserved.
//
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"runtime/pprof"

	"github.com/ghostlytalamaur/codesearch/grep"
	"github.com/ghostlytalamaur/codesearch/regexp"
	log "github.com/sirupsen/logrus"
)

var usageMessage = `usage: cgrep [-c] [-h] [-i] [-l [-0]] [-n] regexp [file...]

cgrep behaves like grep, searching for regexp, an RE2 (nearly PCRE) regular expression.

Options:
  -c           print only a count of selected lines to stdout
  -h           print this help text and exit
  -i           case-insensitive grep
  -l           print only the names of the files containing matches
  -0           print -l matches separated by NUL ('\0') character
  -n           print each output line preceded by its relative line number in
               the file, starting at 1

Note that as per Go's flag parsing convention, the options cannot be combined.
For example, the option pair -i -n cannot be abbreviated to -in.

The -0 flag is only meaningful with the -l option. It outputs the results
separated by NUL ('\0') character instead of the standard NL ('\n') character.
`

func usage() {
	fmt.Fprintf(os.Stderr, usageMessage)
	os.Exit(2)
}

var (
	iflag      = flag.Bool("i", false, "case-insensitive match")
	cpuProfile = flag.String("cpuprofile", "", "write cpu profile to this file")
)

func main() {
	var g grep.Grep
	var grepParams grep.Params
	var formatParams grep.ResultFormatParams
	grepParams.AddFlags()
	formatParams.AddFlags()
	g.Params = grepParams
	flag.Usage = usage
	flag.Parse()
	args := flag.Args()
	if len(args) == 0 {
		flag.Usage()
	}

	log.SetLevel(log.ErrorLevel)
	log.SetFormatter(&log.TextFormatter{
		FullTimestamp:          true,
		DisableLevelTruncation: true,
		ForceColors:            true,
	})

	if *cpuProfile != "" {
		f, err := os.Create(*cpuProfile)
		if err != nil {
			log.Fatal(err)
		}
		defer f.Close()
		pprof.StartCPUProfile(f)
		defer pprof.StopCPUProfile()
	}

	pat := "(?m)" + args[0]
	if *iflag {
		pat = "(?i)" + pat
	}
	re, err := regexp.Compile(pat)
	if err != nil {
		log.Fatal(err)
	}
	g.Regexp = re
	var matched bool
	if len(args) == 1 {
		for r := range g.Reader(context.Background(), "<standard input>", os.Stdin) {
			fmt.Print(r.Format(&formatParams))
			matched = true
		}
	} else {
		for _, arg := range args[1:] {
			for r := range g.File(context.Background(), arg) {
				fmt.Print(r.Format(&formatParams))
				matched = true
			}
		}
	}
	if !matched {
		os.Exit(1)
	}
}
