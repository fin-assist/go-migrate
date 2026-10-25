package migrate

import (
	"context"
	"sync"
	"time"
)

type ownedConnection interface{ Close(context.Context) error }

// openingConnections records only connections opened by our own pool while the
// upstream driver initializes. WithInstance fails to return its borrowed sql.Conn
// when version-table initialization fails, so sql.DB.Close alone cannot close it.
type openingConnections struct {
	mu               sync.Mutex
	connections      []ownedConnection
	finished, failed bool
}

func (o *openingConnections) capture(c ownedConnection) {
	o.mu.Lock()
	if !o.finished {
		o.connections = append(o.connections, c)
		o.mu.Unlock()
		return
	}
	failed := o.failed
	o.mu.Unlock()
	if failed {
		closeOwned(c)
	}
}
func (o *openingConnections) finish(failed bool) {
	o.mu.Lock()
	o.finished = true
	o.failed = failed
	connections := o.connections
	o.connections = nil
	o.mu.Unlock()
	if failed {
		for _, c := range connections {
			closeOwned(c)
		}
	}
}
func closeOwned(c ownedConnection) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = c.Close(ctx)
}
