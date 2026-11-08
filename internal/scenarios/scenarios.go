// Package scenarios holds the demo's goroutine leaks and their fixes.
//
// Each scenario is a small, realistic bug: a function that returns while a
// goroutine it started can never be woken. The runtime's goroutine leak
// profile (Go 1.27) finds such a goroutine because the channel, mutex or
// WaitGroup it waits on is no longer reachable from any goroutine that could
// still run. Every leak has a fixed twin that leaves nothing behind.
//
// The page shows the code between the "demo:begin" and "demo:end" markers,
// and highlights the line each leaked goroutine is stuck on, so keep those
// blocks readable: they are the lesson.
package scenarios

import (
	"context"
	_ "embed"
	"errors"
	"sync"
	"time"
)

//go:embed scenarios.go
var Source string

// File is this file's base name, as it appears in stack frames.
const File = "scenarios.go"

var errRefresh = errors.New("upstream timeout")

// demo:begin send-leaky
// fetchWithTimeout gives up after 1 ms, but the worker it started still
// tries to hand over its result on an unbuffered channel nobody reads.
func fetchWithTimeout() {
	result := make(chan int)
	go func() {
		time.Sleep(5 * time.Millisecond) // slow work
		result <- 42                     // blocks forever: the caller left
	}()
	select {
	case <-result:
	case <-time.After(time.Millisecond):
	}
}

// demo:end

// demo:begin send-fixed
// Fixed: a buffer of one lets the worker drop its result and exit.
func fetchWithTimeoutFixed() {
	result := make(chan int, 1)
	go func() {
		time.Sleep(5 * time.Millisecond)
		result <- 42 // never blocks
	}()
	select {
	case <-result:
	case <-time.After(time.Millisecond):
	}
}

// demo:end

// demo:begin waitgroup-leaky
// processBatch returns early on an error path without calling Done, so the
// goroutine waiting for the batch waits forever.
func processBatch() {
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		if err := errRefresh; err != nil {
			return // forgot wg.Done()
		}
		wg.Done()
	}()
	go func() {
		wg.Wait() // blocks forever
	}()
}

// demo:end

// demo:begin waitgroup-fixed
// Fixed: defer wg.Done() runs on every return path.
func processBatchFixed() {
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := errRefresh; err != nil {
			return
		}
	}()
	go func() {
		wg.Wait()
	}()
}

// demo:end

// demo:begin mutex-leaky
// refreshCache takes the lock and returns on an error without unlocking,
// so the next reader waits on the lock forever.
func refreshCache() {
	var mu sync.Mutex
	refresh := func() error {
		mu.Lock()
		if err := errRefresh; err != nil {
			return err // forgot mu.Unlock()
		}
		mu.Unlock()
		return nil
	}
	_ = refresh()
	go func() {
		mu.Lock() // blocks forever
		mu.Unlock()
	}()
}

// demo:end

// demo:begin mutex-fixed
// Fixed: defer mu.Unlock() releases the lock on every return path.
func refreshCacheFixed() {
	var mu sync.Mutex
	refresh := func() error {
		mu.Lock()
		defer mu.Unlock()
		if err := errRefresh; err != nil {
			return err
		}
		return nil
	}
	_ = refresh()
	go func() {
		mu.Lock()
		mu.Unlock()
	}()
}

// demo:end

// demo:begin select-leaky
// firstResult takes no result and returns, but never closes done, so the
// producer waits in its select forever.
func firstResult() {
	results := make(chan int)
	done := make(chan struct{})
	go func() {
		select { // blocks forever
		case results <- 42:
		case <-done:
		}
	}()
	// The caller moves on without reading results or closing done.
}

// demo:end

// demo:begin select-fixed
// Fixed: closing done on the way out releases the producer.
func firstResultFixed() {
	results := make(chan int)
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case results <- 42:
		case <-done:
		}
	}()
}

// demo:end

// demo:begin context-leaky
// startWorker throws away the cancel function, so the worker's context is
// never cancelled and its loop never ends.
func startWorker() {
	ctx, cancel := context.WithCancel(context.Background())
	_ = cancel // kept "for later", never called
	jobs := make(chan int)
	go func() {
		for {
			select { // blocks forever
			case <-ctx.Done():
				return
			case <-jobs:
			}
		}
	}()
}

// demo:end

// demo:begin context-fixed
// Fixed: defer cancel() stops the worker when the caller returns.
func startWorkerFixed() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	jobs := make(chan int)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-jobs:
			}
		}
	}()
}

// demo:end

// demo:begin blindspot-leaky
// parked is a package-level channel nobody ever sends on. A goroutine
// waiting on it is stuck for good, but the channel is still reachable from
// a global, so the leak profile cannot tell it from a healthy waiter.
var parked = make(chan int)

func waitForSignal() {
	go func() {
		<-parked // stuck, and not reported
	}()
}

// demo:end

// demo:begin healthy
// jobs feeds a small pool of idle workers. They block too, but jobs is
// reachable, so they are waiting, not leaked. The old goroutine dump shows
// them exactly like the leaks.
var jobs = make(chan func())

// StartPool starts the idle workers once per process.
func StartPool(n int) {
	for range n {
		go func() {
			for job := range jobs {
				job()
			}
		}()
	}
}

// demo:end

// Scenario is one bug and, unless it is the blind spot, its fix.
type Scenario struct {
	ID      string `json:"id"`
	Title   string `json:"title"`
	Bug     string `json:"bug"`
	Fix     string `json:"fix,omitempty"`
	Primit  string `json:"primitive"`
	Detects bool   `json:"detected"`
	// Func is the leaky function's name as it appears in stack frames.
	Func  string `json:"func"`
	leaky func()
	fixed func()
}

// All lists the scenarios in page order.
var All = []*Scenario{
	{ID: "send", Title: "Send with no receiver", Primit: "channel send", Detects: true,
		Bug:   "A request times out, but its worker still sends its result on an unbuffered channel nobody reads.",
		Fix:   "Give the channel a buffer of one so the worker can drop its result and exit.",
		Func:  "fetchWithTimeout",
		leaky: fetchWithTimeout, fixed: fetchWithTimeoutFixed},
	{ID: "waitgroup", Title: "Forgotten wg.Done()", Primit: "sync.WaitGroup", Detects: true,
		Bug:   "An error path returns before wg.Done(), so the goroutine waiting on the WaitGroup waits forever.",
		Fix:   "defer wg.Done() at the top of the goroutine.",
		Func:  "processBatch",
		leaky: processBatch, fixed: processBatchFixed},
	{ID: "mutex", Title: "Mutex never unlocked", Primit: "sync.Mutex", Detects: true,
		Bug:   "An error path returns while holding the lock, so the next reader blocks on Lock forever.",
		Fix:   "defer mu.Unlock() straight after Lock.",
		Func:  "refreshCache",
		leaky: refreshCache, fixed: refreshCacheFixed},
	{ID: "select", Title: "Select that can never fire", Primit: "select", Detects: true,
		Bug:   "The caller stops listening and never closes done, so the producer's select has no case left that can run.",
		Fix:   "defer close(done) in the caller.",
		Func:  "firstResult",
		leaky: firstResult, fixed: firstResultFixed},
	{ID: "context", Title: "Context never cancelled", Primit: "context", Detects: true,
		Bug:   "The cancel function is thrown away, so the worker's ctx.Done() never closes and its loop never ends.",
		Fix:   "Keep cancel and defer cancel().",
		Func:  "startWorker",
		leaky: startWorker, fixed: startWorkerFixed},
	{ID: "blindspot", Title: "The blind spot", Primit: "global channel", Detects: false,
		Bug:  "A goroutine waits on a package-level channel nobody sends on. It is stuck for good, but the channel is still reachable from a global, so the profile cannot call it leaked.",
		Func: "waitForSignal", leaky: waitForSignal},
}

// ByID finds a scenario.
func ByID(id string) (*Scenario, bool) {
	for _, s := range All {
		if s.ID == id {
			return s, true
		}
	}
	return nil, false
}

// HasFix reports whether the scenario has a fixed twin.
func (s *Scenario) HasFix() bool { return s.fixed != nil }

// Run calls the leaky or fixed version n times.
func (s *Scenario) Run(fixed bool, n int) {
	f := s.leaky
	if fixed {
		f = s.fixed
	}
	if f == nil {
		return
	}
	for range n {
		f()
	}
}
