package channelwriter

import "sync/atomic"

// New return new ChannelWriter with given buffer
func New(buffer int) *ChannelWriter {
	return &ChannelWriter{
		channel: make(chan string, buffer),
	}
}

// ChannelWriter io.Writer which writes to given string channel
type ChannelWriter struct {
	channel  chan string
	isClosed int32
}

func (w *ChannelWriter) Write(p []byte) (n int, err error) {
	w.channel <- string(p)
	return len(p), nil
}

// Chan return underlying string channel
func (w *ChannelWriter) Chan() chan string {
	return w.channel
}

// Close closes underlying string channel
func (w *ChannelWriter) Close() {
	if atomic.CompareAndSwapInt32(&w.isClosed, 0, 1) {
		close(w.channel)
	}
}

// IsClosed return true if underlying string channel was closed by Close() call
func (w *ChannelWriter) IsClosed() bool {
	return atomic.LoadInt32(&w.isClosed) == 1
}
