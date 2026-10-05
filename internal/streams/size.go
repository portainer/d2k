package streams

import (
	"sync"

	"k8s.io/client-go/tools/remotecommand"
)

// SizeQueue feeds terminal resizes to an exec or attach stream. It keeps only the
// latest size, as a terminal only cares about its current one.
type SizeQueue struct {
	sizes chan remotecommand.TerminalSize
	once  sync.Once
	done  chan struct{}
}

// NewSizeQueue returns an empty SizeQueue.
func NewSizeQueue() *SizeQueue {
	return &SizeQueue{sizes: make(chan remotecommand.TerminalSize, 1), done: make(chan struct{})}
}

// Push sets the terminal size; a size not yet applied is replaced.
func (q *SizeQueue) Push(height, width uint) {
	size := remotecommand.TerminalSize{Height: uint16(height), Width: uint16(width)}
	select {
	case <-q.done:
		return
	default:
	}
	select {
	case <-q.sizes: // drop a size not yet applied
	default:
	}
	select {
	case q.sizes <- size:
	default:
	}
}

// Next blocks until a resize, or returns nil once the exec has ended.
func (q *SizeQueue) Next() *remotecommand.TerminalSize {
	select {
	case size := <-q.sizes:
		return &size
	case <-q.done:
		return nil
	}
}

// Close ends the queue: Next returns nil and Push does nothing.
func (q *SizeQueue) Close() { q.once.Do(func() { close(q.done) }) }
