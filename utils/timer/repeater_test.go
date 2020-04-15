// (c) 2019-2020, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package timer

import (
	"testing"
	"time"

	"github.com/sasha-s/go-deadlock"
)

func TestRepeater(t *testing.T) {
	wg := deadlock.WaitGroup{}
	wg.Add(2)

	val := new(int)
	repeater := NewRepeater(func() {
		if *val < 2 {
			wg.Done()
			*val++
		}
	}, time.Millisecond)
	go repeater.Dispatch()

	wg.Wait()
	repeater.Stop()
}
