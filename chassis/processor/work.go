package processor

import "sync"

// Tracker counts pipeline work in flight so shutdown can let it finish before
// cancelling it. The server's bus loop tracks its request goroutines with one;
// Unit.Work tracks the work that outlives its request — a continuation's
// detached tail, a local async op, a worker callback's resume — which no
// request goroutine waits for.
//
// Unlike sync.WaitGroup, Begin may race a waiter at a zero count: a draining
// node keeps admitting internal work while shutdown waits. Nil-safe, so a Unit
// built without New (unit tests) tracks nothing.
type Tracker struct {
	mu   sync.Mutex
	n    int
	idle chan struct{} // closed while n == 0; replaced when work begins
}

var closedIdle = func() chan struct{} {
	c := make(chan struct{})
	close(c)
	return c
}()

// NewTracker returns an idle tracker.
func NewTracker() *Tracker { return &Tracker{idle: closedIdle} }

// Begin counts one unit of work until the returned func is called. Calling
// it more than once is a no-op.
func (t *Tracker) Begin() (end func()) {
	if t == nil {
		return func() {}
	}
	t.mu.Lock()
	if t.n == 0 {
		t.idle = make(chan struct{})
	}
	t.n++
	t.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			t.mu.Lock()
			t.n--
			if t.n == 0 {
				close(t.idle)
			}
			t.mu.Unlock()
		})
	}
}

// Count is the work in flight now.
func (t *Tracker) Count() int {
	if t == nil {
		return 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.n
}

// Idle returns a channel that is closed once nothing is in flight: already
// closed when the count is zero, otherwise closed when it next reaches zero.
func (t *Tracker) Idle() <-chan struct{} {
	if t == nil {
		return closedIdle
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.n == 0 {
		return closedIdle
	}
	return t.idle
}
