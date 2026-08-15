package audit

import (
	"context"
	"sync"
	"time"
)

// memQueue is an in-memory Queue fake with BRPOPLPUSH blocking
// semantics. It is shared by the sink and worker tests.
type memQueue struct {
	mu     sync.Mutex
	lists  map[string][][]byte
	pushes map[string]int
	sig    chan struct{} // closed (and replaced) on every RPush to wake blockers
}

func newMemQueue() *memQueue {
	return &memQueue{
		lists:  make(map[string][][]byte),
		pushes: make(map[string]int),
		sig:    make(chan struct{}),
	}
}

func (q *memQueue) RPush(ctx context.Context, key string, payload []byte) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	q.mu.Lock()
	q.lists[key] = append(q.lists[key], payload)
	q.pushes[key]++
	close(q.sig)
	q.sig = make(chan struct{})
	q.mu.Unlock()
	return nil
}

func (q *memQueue) BRPopLPush(ctx context.Context, src, dst string, timeout time.Duration) ([]byte, error) {
	for {
		q.mu.Lock()
		if l := q.lists[src]; len(l) > 0 {
			v := l[len(l)-1]
			q.lists[src] = l[:len(l)-1]
			q.lists[dst] = append([][]byte{v}, q.lists[dst]...)
			q.mu.Unlock()
			return v, nil
		}
		sig := q.sig
		q.mu.Unlock()
		if timeout <= 0 {
			return nil, nil // non-blocking poll
		}
		t := time.NewTimer(timeout)
		select {
		case <-ctx.Done():
			t.Stop()
			return nil, ctx.Err()
		case <-sig:
			t.Stop()
		case <-t.C:
			return nil, nil // timeout: no element
		}
	}
}

func (q *memQueue) LRem(ctx context.Context, key string, count int64, payload []byte) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	l := q.lists[key]
	removed := int64(0)
	out := l[:0]
	for _, v := range l {
		if removed < count && string(v) == string(payload) {
			removed++
			continue
		}
		out = append(out, v)
	}
	q.lists[key] = out
	return nil
}

func (q *memQueue) list(key string) [][]byte {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := make([][]byte, len(q.lists[key]))
	copy(out, q.lists[key])
	return out
}

func (q *memQueue) len(key string) int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.lists[key])
}

// pushCount reports how many RPush calls landed on key (used to observe
// requeues).
func (q *memQueue) pushCount(key string) int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.pushes[key]
}
