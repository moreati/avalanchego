// (c) 2019-2020, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package timer

import (
	"container/list"
	"time"

	"github.com/sasha-s/go-deadlock"
)

// TimedMeter is a meter that discards old events
type TimedMeter struct {
	lock deadlock.Mutex
	// Amount of time to keep a tick
	Duration time.Duration
	// TODO: Currently this list has an entry for each tick... This isn't really
	// sustainable at high tick numbers. We should be batching ticks with
	// similar times into the same bucket.
	tickList *list.List
}

// Tick implements the Meter interface
func (tm *TimedMeter) Tick() {
	tm.lock.Lock()
	defer tm.lock.Unlock()

	tm.tick()
}

// Ticks implements the Meter interface
func (tm *TimedMeter) Ticks() int {
	tm.lock.Lock()
	defer tm.lock.Unlock()

	return tm.ticks()
}

func (tm *TimedMeter) init() {
	if tm.tickList == nil {
		tm.tickList = list.New()
	}
}

func (tm *TimedMeter) tick() {
	tm.init()
	tm.tickList.PushBack(time.Now())
}

func (tm *TimedMeter) ticks() int {
	tm.init()

	timeBound := time.Now().Add(-tm.Duration)
	// removeExpiredHead returns false once there is nothing left to remove
	for tm.removeExpiredHead(timeBound) {
	}
	return tm.tickList.Len()
}

// Returns true if the head was removed, false otherwise
func (tm *TimedMeter) removeExpiredHead(t time.Time) bool {
	if tm.tickList.Len() == 0 {
		return false
	}

	head := tm.tickList.Front()
	headTime := head.Value.(time.Time)

	if headTime.Before(t) {
		tm.tickList.Remove(head)
		return true
	}
	return false
}
