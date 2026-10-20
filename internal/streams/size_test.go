package streams

import "testing"

func TestSizeQueueKeepsLatestAndEnds(t *testing.T) {
	q := NewSizeQueue()
	q.Push(24, 80)
	q.Push(40, 120)
	if size := q.Next(); size == nil || size.Height != 40 || size.Width != 120 {
		t.Fatalf("Next() = %+v, want the latest size 40x120", size)
	}
	q.Close()
	if size := q.Next(); size != nil {
		t.Fatalf("Next() after close = %+v, want nil", size)
	}
	q.Push(1, 1) // after close: no panic, no block
}
