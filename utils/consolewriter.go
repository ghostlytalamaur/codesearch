package utils

import (
	"fmt"
	"io"
	"os"
	"sync"

	"github.com/ghostlytalamaur/codesearch/utils/channelwriter"
)

var (
	instance *consoleWriter
	once     sync.Once
	done     chan bool
	wg       sync.WaitGroup
)

// GetConsoleWriter return instance of ConsoleWriter,
// that may be safely used concurrently for write to console
func GetConsoleWriter() ConsoleWriter {
	once.Do(func() {
		done = make(chan bool)
		out := channelwriter.New(1000)
		wg.Add(1)
		go func() {
			for s := range out.Chan() {
				fmt.Fprint(os.Stdout, s)
			}
			wg.Done()
		}()

		wg.Add(1)
		err := channelwriter.New(1000)
		go func() {
			for s := range err.Chan() {
				fmt.Fprint(os.Stderr, s)
			}
			wg.Done()
		}()

		go func() {
			select {
			case <-done:
				{
					out.Close()
					err.Close()
				}
			}
		}()

		instance = &consoleWriter{
			out: out,
			err: err,
		}
	})
	return instance
}

// DoneConsoleWriter closes channels to os.Stdout and os.Stderr
// and waits when all remaining data will be writen.
//
// Must be last call in application.
func DoneConsoleWriter() {
	if done != nil {
		done <- true
		wg.Wait()
	}
}

type consoleWriter struct {
	out io.Writer
	err io.Writer
}

func (w *consoleWriter) Out() io.Writer {
	return w.out
}

func (w *consoleWriter) Err() io.Writer {
	return w.err
}
