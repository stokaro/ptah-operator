package harness

import (
	"sync/atomic"
	"testing"
	"time"
)

func TestParallelPairOverlapsAndJoinsBothLanes(t *testing.T) {
	started := make(chan struct{}, 2)
	release := make(chan struct{})
	go func() {
		defer close(release)
		for range 2 {
			select {
			case <-started:
			case <-time.After(5 * time.Second):
				return
			}
		}
	}()
	var entered, finished atomic.Int32
	body := func(t *testing.T) {
		entered.Add(1)
		started <- struct{}{}
		<-release
		if entered.Load() != 2 {
			t.Fatal("the independent lanes did not overlap")
		}
		finished.Add(1)
	}
	if !ParallelPair(t, "pair", [2]ParallelCase{{Name: "first", Run: body}, {Name: "second", Run: body}}) {
		t.Fatal("the independent lanes failed")
	}
	if finished.Load() != 2 {
		t.Fatal("the parent resumed before both lanes finished")
	}
}
