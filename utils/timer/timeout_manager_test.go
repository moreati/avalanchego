// (c) 2019-2020, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package timer

import (
	"testing"
	"time"

	"github.com/sasha-s/go-deadlock"

	"github.com/ava-labs/gecko/ids"
)

func TestTimeoutManager(t *testing.T) {
	wg := deadlock.WaitGroup{}
	wg.Add(2)
	defer wg.Wait()

	tm := TimeoutManager{}
	tm.Initialize(time.Millisecond)
	go tm.Dispatch()

	tm.Put(ids.NewID([32]byte{}), wg.Done)
	tm.Put(ids.NewID([32]byte{1}), wg.Done)
}
