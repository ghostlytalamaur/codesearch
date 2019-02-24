// Copyright 2011 The Go Authors.  All rights reserved.
// Copyright 2013-2016 Manpreet Singh ( junkblocker@yahoo.com ). All rights reserved.
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
	"strings"
	"sync"

	"github.com/ghostlytalamaur/codesearch/grep"
	"github.com/ghostlytalamaur/codesearch/index"
	"github.com/ghostlytalamaur/codesearch/regexp"
	"github.com/ghostlytalamaur/codesearch/utils"
	log "github.com/sirupsen/logrus"
)

const usageMessage = `usage: csearch [options] regexp

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
	grepParams        grep.Params
	formatParams      grep.ResultFormatParams
	fFlag             string // ;           = flag.String("f", "", "search only files with names matching this regexp")
	fExcludeFlag      string // ; //     = flag.String("fexclude", "", "search only files with names not matching this regexp")
	iFlag             bool   //           = flag.Bool("i", false, "case-insensitive search")
	bruteFlag         bool   //      = flag.Bool("brute", false, "brute force - search all files in index")
	cpuProfile        string //     = flag.String("cpuprofile", "", "write cpu profile to this file")
	indexPath         string //      = flag.String("indexpath", "", "specifies index path")
	maxCount          int64  //       = flag.Int64("m", 0, "specified maximum number of search results")
	maxCountPerFile   int64  // = flag.Int64("M", 0, "specified maximum number of search results per file")
	filePathsStr      string //   = flag.String("filepaths", "", "search only files in specified paths separated by |")
	ignorePathsCase   bool   //  = flag.Bool("ignorepathscase", false, "Ignore case of paths specified by filepaths param")
	extStatus         bool   //      = flag.Bool("extstatus", false, "Print additional status info")
	noMultiThread     bool
	pattern           string
	printMatchesCount bool // C flag - print count of matches
	verboseFlag       bool //     = flag.Bool("verbose", false, "print extra information")
}

// AddFlags register command line flags to parse
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
	flag.BoolVar(&p.noMultiThread, "nomultithread", false, "use multiple threads")
	flag.BoolVar(&p.printMatchesCount, "c", false, "print match counts only")
	p.formatParams.AddFlags()
	p.grepParams.AddFlags()
}

func prepareIndex(params *CSearchParams) (*index.Index, error) {
	if params.indexPath != "" {
		err := os.Setenv("CSEARCHINDEX", params.indexPath)
		if err != nil {
			return nil, err
		}
	}

	ix := index.Open(index.File())
	ix.Verbose = params.verboseFlag
	return ix, nil
}

func produceFiles(ctx context.Context, params *CSearchParams, buffer int) <-chan string {
	files := make(chan string, buffer)
	go func() {
		defer close(files)

		re, err := regexp.Compile(params.pattern)
		if err != nil {
			log.Fatal(err)
		}

		ix, err := prepareIndex(params)
		if err != nil {
			log.Fatal(err)
		}

		q := index.RegexpQuery(re.Syntax)
		log.Tracef("Query: %s\n", q)

		var post []uint32
		if params.bruteFlag {
			post = ix.PostingQuery(&index.Query{Op: index.QAll})
		} else {
			post = ix.PostingQuery(q)
		}

		log.Tracef("Post query identified %d possible files\n", len(post))

		var fre *regexp.Regexp
		if params.fFlag != "" {
			fre, err = regexp.Compile(params.fFlag)
			if err != nil {
				log.Panic(err)
			}
		}

		var fExcludeRe *regexp.Regexp
		if params.fExcludeFlag != "" {
			fExcludeRe, err = regexp.Compile(params.fExcludeFlag)
			if err != nil {
				log.Panic(err)
			}
		}

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
				select {
				case files <- name:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return files
}

func searchWorker(ctx context.Context, params *CSearchParams,
	files <-chan string, output chan<- grep.Result) bool {
	localRe, _ := regexp.Compile(params.pattern)
	localGrep := grep.Grep{
		Params: params.grepParams,
		Regexp: localRe,
	}

	somethingFound := false
	for file := range files {
		select {
		case <-ctx.Done():
			return somethingFound
		default:
		}

		if params.extStatus {
			fmt.Fprintf(utils.GetConsoleWriter().Out(), "Status: search in file %s\n", file)
		}

		lctx, cancel := context.WithCancel(ctx)
		var count int64
		for r := range localGrep.File(lctx, file) {
			count++
			if params.maxCountPerFile <= 0 || count <= params.maxCountPerFile {
				select {
				case <-lctx.Done():
				case output <- r:
					somethingFound = true
				}
			} else {
				log.Tracef("File limit achieved. File: %s", file)
				cancel()
			}
		}
		cancel()
	}

	return somethingFound
}

func performSearch(ctx context.Context, params *CSearchParams, workersCount int) bool {
	lctx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	out := make(chan grep.Result, 1000)
	wg.Add(1)
	go func() {
		defer close(out)
		defer wg.Done()
		var locWg sync.WaitGroup
		files := produceFiles(lctx, params, workersCount)
		for i := 0; i < workersCount; i++ {
			locWg.Add(1)
			go func() {
				defer locWg.Done()
				searchWorker(lctx, params, files, out)
			}()
		}
		locWg.Wait()
	}()

	wg.Add(1)
	var count int64
	go func() {
		defer wg.Done()
		for r := range out {
			count++
			if params.maxCount <= 0 || count <= params.maxCount {
				fmt.Fprintf(utils.GetConsoleWriter().Out(), "%s", r.Format(&params.formatParams))
			} else {
				log.Trace("Global limit achieved.")
				break
			}
		}
		cancel()
	}()

	wg.Wait()
	return count > 0
}

func search(params *CSearchParams) bool {
	numOfWorkers := 10
	if params.noMultiThread {
		numOfWorkers = 1
	}
	ctx := context.TODO()
	return performSearch(ctx, params, numOfWorkers)
}

func main() {
	var params CSearchParams
	params.addFlags()
	flag.Usage = usage
	flag.Parse()
	args := flag.Args()

	if len(args) != 1 || (params.formatParams.PrintFileNamesOnly && params.printMatchesCount) ||
		(params.formatParams.PrintFileNamesOnly && params.maxCountPerFile > 0) ||
		(params.printMatchesCount && params.maxCountPerFile > 0) {
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

	params.pattern = "(?m)" + args[0]
	if params.iFlag {
		params.pattern = "(?i)" + params.pattern
	}

	if params.verboseFlag {
		log.SetLevel(log.TraceLevel)
	} else {
		log.SetLevel(log.InfoLevel)
	}
	log.SetFormatter(&log.TextFormatter{
		FullTimestamp:          true,
		DisableLevelTruncation: true,
		ForceColors:            true,
	})
	if !search(&params) {
		utils.DoneConsoleWriter()
		os.Exit(1)
	}
	utils.DoneConsoleWriter()
	os.Exit(0)
}
