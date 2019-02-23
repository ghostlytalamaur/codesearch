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
	"io"
	"log"
	"os"
	"runtime/pprof"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ghostlytalamaur/codesearch/index"
	"github.com/ghostlytalamaur/codesearch/regexp"
	"github.com/ghostlytalamaur/codesearch/utils"
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
	multithread     bool
	pattern         string
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
	flag.BoolVar(&p.multithread, "multithread", false, "use multiple threads")
	p.grepParams.AddFlags()
}

func selectFiles(re *regexp.Regexp, ix *index.Index, params *CSearchParams) (post []uint32, err error) {
	q := index.RegexpQuery(re.Syntax)
	if params.verboseFlag {
		log.Printf("query: %s\n", q)
	}

	if params.bruteFlag {
		post = ix.PostingQuery(&index.Query{Op: index.QAll})
	} else {
		post = ix.PostingQuery(q)
	}

	if params.verboseFlag {
		log.Printf("post query identified %d possible files\n", len(post))
	}

	var fre *regexp.Regexp
	if params.fFlag != "" {
		fre, err = regexp.Compile(params.fFlag)
		if err != nil {
			return nil, err
		}
	}

	var fExcludeRe *regexp.Regexp
	if params.fExcludeFlag != "" {
		fExcludeRe, err = regexp.Compile(params.fExcludeFlag)
		if err != nil {
			return nil, err
		}
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

	return post, nil
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

func limitResults(ctx context.Context, wg *sync.WaitGroup,
	results chan<- regexp.GrepResult, maxElements int64) (context.Context, chan<- regexp.GrepResult) {

	lctx, cancel := context.WithCancel(ctx)
	res := make(chan regexp.GrepResult)
	wg.Add(1)
	go func() {
		defer wg.Done()
		var count int64
		wasCancelled := false
		for r := range res {
			if maxElements <= 0 || atomic.AddInt64(&count, 1) <= maxElements {
				results <- r
			} else if !wasCancelled {
				cancel()
				wasCancelled = true
			}
		}
	}()
	return lctx, res
}

func searchWorker(ctx context.Context, params *CSearchParams, console utils.ConsoleWriter,
	files <-chan string, output chan<- regexp.GrepResult) bool {
	localRe, _ := regexp.Compile(params.pattern)
	localGrep := regexp.Grep{
		Stdout: console.Out(),
		Stderr: console.Err(),
		Params: params.grepParams,
		Regexp: localRe,
	}
	localGrep.LimitPrintCount(params.maxCount, params.maxCountPerFile)

	somethingFound := false
	var wg sync.WaitGroup
	for file := range files {
		select {
		case <-ctx.Done():
			fmt.Fprintf(console.Err(), "Output is closed. Stop on file %s\n", file)
			return somethingFound
		default:
		}

		lctx, limiter := limitResults(ctx, &wg, output, params.maxCountPerFile)
		if localGrep.File(lctx, file, limiter) {
			somethingFound = true
		}
		close(limiter)

		if params.extStatus {
			fmt.Fprintf(console.Out(), "Status: search in file %s\n", file)
		}
		fmt.Fprintf(console.Err(), "End search in file %s\n", file)
	}

	wg.Wait()
	return somethingFound
}

type prefixWriter struct {
	out io.Writer
}

func (w *prefixWriter) Write(p []byte) (n int, err error) {
	now := time.Now()
	h, m, s := now.Clock()
	y, mn, d := now.Date()
	return fmt.Fprintf(w.out, "[%d/%d/%d %d:%d:%d.%d] %s", y, mn, d, h, m, s, now.Nanosecond(), p)
}

type prefixConsole struct {
	out io.Writer
	err io.Writer
}

func (w *prefixConsole) Out() io.Writer {
	return w.out
}

func (w *prefixConsole) Err() io.Writer {
	return w.err
}

func printResults(ctx context.Context, params *CSearchParams, out io.Writer, results <-chan regexp.GrepResult) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		for r := range results {
			if params.grepParams.PrinterParams.PrintFileNamesOnly {
				if params.grepParams.PrinterParams.PrintNullTerminatedStrings {
					fmt.Fprintf(out, "%s\x00", r.FileName)
				} else {
					fmt.Fprintf(out, "%s\n", r.FileName)
				}
			} else {
				var text string
				if params.grepParams.PrinterParams.PrintFileName {
					text = r.FileName + ":"
				}
				if params.grepParams.PrinterParams.PrintLineNumbers {
					text = fmt.Sprintf("%s%d:", text, r.LineNum)
				}
				fmt.Fprintf(out, "%s%s", text, r.Text)
			}
		}
		close(done)
	}()
	return done
}

func performSearch(ctx context.Context, workersCount int, params *CSearchParams, files <-chan string) bool {
	console := &prefixConsole{
		out: utils.GetConsoleWriter().Out(),
		err: &prefixWriter{utils.GetConsoleWriter().Err()},
	}

	var wg sync.WaitGroup
	var limiterWg sync.WaitGroup
	results := make(chan regexp.GrepResult, 1000)
	lctx, limiter := limitResults(ctx, &limiterWg, results, params.maxCount)
	printDone := printResults(ctx, params, console.Out(), results)
	var matches int32
	for i := 0; i < workersCount; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if searchWorker(lctx, params, console, files, limiter) {
				atomic.AddInt32(&matches, 1)
			}
		}()
	}
	wg.Wait()
	close(limiter)
	limiterWg.Wait()
	close(results)
	<-printDone
	return matches > 1
}

func produceFiles(ctx context.Context, ix *index.Index, ids []uint32, buffer int) <-chan string {
	files := make(chan string, buffer)
	go func() {
		defer close(files)
		for _, fileid := range ids {
			name := ix.Name(fileid)
			select {
			case files <- name:
			case <-ctx.Done():
				return
			}
		}
	}()
	return files
}

func doSearch(params *CSearchParams, ix *index.Index, ids []uint32) bool {
	numOfWorkers := 1
	if params.multithread {
		numOfWorkers = 1
	}

	ctx := context.TODO()
	return performSearch(ctx, numOfWorkers, params, produceFiles(ctx, ix, ids, numOfWorkers))
}

func search() bool {
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

	params.pattern = "(?m)" + args[0]
	if params.iFlag {
		params.pattern = "(?i)" + params.pattern
	}
	re, err := regexp.Compile(params.pattern)
	if err != nil {
		log.Fatal(err)
	}

	ix, err := prepareIndex(&params)
	if err != nil {
		log.Fatal(err)
	}

	post, err := selectFiles(re, ix, &params)
	if err != nil {
		log.Fatal(err)
	}

	allFilesCount := len(post)
	if allFilesCount == 0 {
		log.Fatal("0 files identified with selected params.")
	}
	if params.extStatus {
		fmt.Fprintf(os.Stdout, "Status: Identified %d possible files", len(post))
	}

	return doSearch(&params, ix, post)
}

func main() {
	if !search() {
		utils.DoneConsoleWriter()
		os.Exit(1)
	}
	utils.DoneConsoleWriter()
	os.Exit(0)
}
