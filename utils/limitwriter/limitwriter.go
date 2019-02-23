package limitwriter

import (
	"fmt"
	"io"
	"sync/atomic"
)

// New return new io.Writer which limit Write calls to given given writer
func New(writer io.Writer, limit int64) io.Writer {
	return &limitWriter{
		writer: writer,
		limit:  limit,
	}
}

type limitWriter struct {
	writer   io.Writer
	limit    int64
	printed  int64
	finished int32
}

func (w *limitWriter) Write(p []byte) (n int, err error) {
	if w.limit > 0 {
		if atomic.LoadInt32(&w.finished) > 0 {
			return 0, fmt.Errorf("Max lines achived")
		}

		printedLines := atomic.AddInt64(&w.printed, 1)
		if printedLines-1 >= w.limit {
			atomic.AddInt32(&w.finished, 1)
			return 0, fmt.Errorf("Max lines achived")
		}
	}

	return w.writer.Write(p)
}
