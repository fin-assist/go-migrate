package migrate

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
)

type closingConnection struct {
	closed  atomic.Int32
	bounded atomic.Bool
}

func (c *closingConnection) Close(ctx context.Context) error {
	c.closed.Add(1)
	_, bounded := ctx.Deadline()
	c.bounded.Store(bounded && ctx.Err() == nil)
	return nil
}
func TestUnitOpeningConnectionOwnership(t *testing.T) {
	for _, failed := range []bool{false, true} {
		o := &openingConnections{}
		before, after := &closingConnection{}, &closingConnection{}
		o.capture(before)
		o.finish(failed)
		o.capture(after)
		want := int32(0)
		if failed {
			want = 1
		}
		for _, c := range []*closingConnection{before, after} {
			if c.closed.Load() != want || (failed && !c.bounded.Load()) {
				t.Fatalf("failed=%v closed=%d bounded=%v", failed, c.closed.Load(), c.bounded.Load())
			}
		}
	}
}
func TestUnitOpeningConnectionFailureRacingWithConnect(t *testing.T) {
	o := &openingConnections{}
	connections := make([]*closingConnection, 64)
	var wg sync.WaitGroup
	for i := range connections {
		connections[i] = &closingConnection{}
		wg.Add(1)
		go func() { defer wg.Done(); o.capture(connections[i]) }()
	}
	o.finish(true)
	wg.Wait()
	for _, c := range connections {
		if c.closed.Load() != 1 || !c.bounded.Load() {
			t.Fatalf("connection cleanup count %d", c.closed.Load())
		}
	}
}
