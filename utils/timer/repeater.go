// (c) 2019-2020, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package timer

import (
	"time"

	"github.com/sasha-s/go-deadlock"
)

// Repeater ...
type Repeater struct {
	handler func()
	timeout chan struct{}

	lock      deadlock.Mutex
	wg        deadlock.WaitGroup
	finished  bool
	frequency time.Duration
}

// NewRepeater ...
func NewRepeater(handler func(), frequency time.Duration) *Repeater {
	repeater := &Repeater{
		handler:   handler,
		timeout:   make(chan struct{}, 1),
		frequency: frequency,
	}
	repeater.wg.Add(1)

	return repeater
}

// Stop ...
func (r *Repeater) Stop() {
	r.lock.Lock()
	if !r.finished {
		defer r.wg.Wait()
	}
	defer r.lock.Unlock()

	r.finished = true
	r.reset()
}

// Dispatch ...
func (r *Repeater) Dispatch() {
	r.lock.Lock()
	defer r.lock.Unlock()
	defer r.wg.Done()

	timer := time.NewTimer(r.frequency)
	cleared := false
	for !r.finished {
		r.lock.Unlock()

		cleared = false
		select {
		case <-r.timeout:
		case <-timer.C:
			cleared = true
		}

		if !timer.Stop() && !cleared {
			<-timer.C
		}

		if cleared {
			r.handler()
		}

		timer.Reset(r.frequency)

		r.lock.Lock()
	}
}

func (r *Repeater) reset() {
	select {
	case r.timeout <- struct{}{}:
	default:
	}
}
