// Copyright 2011 The Go Authors.  All rights reserved.
// Copyright 2013-2016 Manpreet Singh ( junkblocker@yahoo.com ). All rights reserved.
//
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"runtime/pprof"
	"strings"

	"github.com/ghostlytalamaur/codesearch/index"
	"github.com/ghostlytalamaur/codesearch/regexp"
)

var usageMessage = `usage: csearch [options] regexp

Options:

  -c           print only a count of selected lines to stdout
               (Not meaningful with -l or -M modes)
  -f PATHREGEXP
			   search only files with names matching this regexp
  -fexclude    PATHREGEXP
               search only files with names not matching this regexp
  -h           print this help text and exit
  -i           case-insensitive search
  -l           print only the names of the files containing matches
               (Not meaningful with -c or -M modes)
  -0           print -l matches separated by NUL ('\0') character
  -m MAXCOUNT  limit search output results to MAXCOUNT (0: no limit)
  -M MAXCOUNT  limit search output results to MAXCOUNT per file (0: no limit)
               (Not allowed with -c or -l modes)
  -n           print each output line preceded by its relative line number in
               the file, starting at 1
  -indexpath FILE
               use specified FILE as the index path. Overrides $CSEARCHINDEX.
  -addlines N
               print N additional lines for each search result 
  -filepaths FILEPATHS
               search only files in specified paths separated by |
  -ignorepathscase
               Ignore case of paths specified by filepaths param
  -re2         Use Go regexp engine
  -verbose     print extra information
  -extstatus   print additional status information
  -brute       brute force - search all files in index
  -cpuprofile FILE
               write CPU profile to FILE

As per Go's flag parsing convention, the flags cannot be combined: the option
pair -i -n cannot be abbreviated to -in.

csearch behaves like grep over all indexed files, searching for regexp,
an RE2 (nearly PCRE) regular expression.

Csearch relies on the existence of an up-to-date index created ahead of time.
To build or rebuild the index that csearch uses, run:

	cindex path...

where path... is a list of directories or individual files to be included in
the index. If no index exists, this command creates one.  If an index already
exists, cindex overwrites it.  Run cindex -help for more.

csearch uses the index stored in $CSEARCHINDEX or, if that variable is unset or
empty, $HOME/.csearchindex.
`

func usage() {
	fmt.Fprintf(os.Stderr, usageMessage)
	os.Exit(2)
}

// CSearchParams - params for code search
type CSearchParams struct {
	fFlag           string // ;           = flag.String("f", "", "search only files with names matching this regexp")
	fExcludeFlag    string // ; //     = flag.String("fexclude", "", "search only files with names not matching this regexp")
	iFlag           bool   //           = flag.Bool("i", false, "case-insensitive search")
	verboseFlag     bool   //     = flag.Bool("verbose", false, "print extra information")
	bruteFlag       bool   //      = flag.Bool("brute", false, "brute force - search all files in index")
	cpuProfile      string //     = flag.String("cpuprofile", "", "write cpu profile to this file")
	indexPath       string //      = flag.String("indexpath", "", "specifies index path")
	maxCount        int64  //       = flag.Int64("m", 0, "specified maximum number of search results")
	maxCountPerFile int64  // = flag.Int64("M", 0, "specified maximum number of search results per file")
	filePathsStr    string //   = flag.String("filepaths", "", "search only files in specified paths separated by |")
	ignorePathsCase bool   //  = flag.Bool("ignorepathscase", false, "Ignore case of paths specified by filepaths param")
	extStatus       bool   //      = flag.Bool("extstatus", false, "Print additional status info")
	grepParams      regexp.GrepParams
}

func (p *CSearchParams) addFlags() {
	flag.StringVar(&p.fFlag, "f", "", "search only files with names matching this regexp")
	flag.StringVar(&p.fExcludeFlag, "fexclude", "", "search only files with names not matching this regexp")
	flag.BoolVar(&p.iFlag, "i", false, "case-insensitive search")
	flag.BoolVar(&p.verboseFlag, "verbose", false, "print extra information")
	flag.BoolVar(&p.bruteFlag, "brute", false, "brute force - search all files in index")
	flag.StringVar(&p.cpuProfile, "cpuprofile", "", "write cpu profile to this file")
	flag.StringVar(&p.indexPath, "indexpath", "", "specifies index path")
	flag.Int64Var(&p.maxCount, "m", 0, "specified maximum number of search results")
	flag.Int64Var(&p.maxCountPerFile, "M", 0, "specified maximum number of search results per file")
	flag.StringVar(&p.filePathsStr, "filepaths", "", "search only files in specified paths separated by |")
	flag.BoolVar(&p.ignorePathsCase, "ignorepathscase", false, "Ignore case of paths specified by filepaths param")
	flag.BoolVar(&p.extStatus, "extstatus", false, "Print additional status info")
	p.grepParams.AddFlags()
}

func Main() bool {
	var params CSearchParams
	params.addFlags()
	flag.Usage = usage
	flag.Parse()
	args := flag.Args()

	if len(args) != 1 || (params.grepParams.L && params.grepParams.C) ||
		(params.grepParams.L && params.maxCountPerFile > 0) || (params.grepParams.C && params.maxCountPerFile > 0) {
		usage()
	}

	if params.cpuProfile != "" {
		f, err := os.Create(params.cpuProfile)
		if err != nil {
			log.Fatal(err)
		}
		defer f.Close()
		pprof.StartCPUProfile(f)
		defer pprof.StopCPUProfile()
	}

	if params.indexPath != "" {
		err := os.Setenv("CSEARCHINDEX", params.indexPath)
		if err != nil {
			log.Fatal(err)
		}
	}

	pat := "(?m)" + args[0]
	if params.iFlag {
		pat = "(?i)" + pat
	}
	re, err := regexp.Compile(pat)
	if err != nil {
		log.Fatal(err)
	}
	var fre *regexp.Regexp
	if params.fFlag != "" {
		fre, err = regexp.Compile(params.fFlag)
		if err != nil {
			log.Fatal(err)
		}
	}

	var fExcludeRe *regexp.Regexp
	if params.fExcludeFlag != "" {
		fExcludeRe, err = regexp.Compile(params.fExcludeFlag)
		if err != nil {
			log.Fatal(err)
		}
	}
	q := index.RegexpQuery(re.Syntax)
	if params.verboseFlag {
		log.Printf("query: %s\n", q)
	}

	ix := index.Open(index.File())
	ix.Verbose = params.verboseFlag
	var post []uint32
	if params.bruteFlag {
		post = ix.PostingQuery(&index.Query{Op: index.QAll})
	} else {
		post = ix.PostingQuery(q)
	}
	if params.verboseFlag {
		log.Printf("post query identified %d possible files\n", len(post))
	}

	if fre != nil || fExcludeRe != nil || params.filePathsStr != "" {
		fnames := make([]uint32, 0, len(post))
		pathsStr := params.filePathsStr
		if params.ignorePathsCase {
			pathsStr = strings.ToLower(pathsStr)
		}
		filePaths := strings.Split(pathsStr, "|")
		for _, fileid := range post {
			name := ix.Name(fileid)
			if fre != nil {
				start, end := fre.MatchString2(name, true, true)
				if start > end {
					continue
				}
			}

			if fExcludeRe != nil {
				start, end := fExcludeRe.MatchString2(name, true, true)
				if start <= end {
					continue
				}
			}

			shouldAppend := false
			for _, filePath := range filePaths {
				if params.ignorePathsCase {
					shouldAppend = strings.Contains(strings.ToLower(name), filePath)
				} else {
					shouldAppend = strings.Contains(name, filePath)
				}
				if shouldAppend {
					break
				}
			}

			if shouldAppend {
				fnames = append(fnames, fileid)
			}
		}

		if params.verboseFlag {
			log.Printf("filename regexp matched %d files\n", len(fnames))
		}
		post = fnames
	}

	allFilesCount := len(post)
	if allFilesCount == 0 {
		log.Fatal("0 files identified with selected params.")
	}
	if params.extStatus {
		fmt.Fprintf(os.Stdout, "Status: Identified %d possible files", len(post))
	}
	g := regexp.Grep{
		Stdout: os.Stdout,
		Stderr: os.Stderr,
		Params: params.grepParams,
		Regexp: re,
	}
	g.LimitPrintCount(params.maxCount, params.maxCountPerFile)
	for i, fileid := range post {
		name := ix.Name(fileid)
		if params.extStatus {
			fmt.Fprintf(os.Stdout, "Status: Searching in %d/%d file %s\n", i, allFilesCount, name)
		}
		g.File(name)
		// short circuit here too
		if g.Done {
			break
		}
	}

	return g.Match
}

func main() {
	if !Main() {
		os.Exit(1)
	}
	os.Exit(0)
}
