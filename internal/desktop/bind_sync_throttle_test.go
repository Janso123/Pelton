package desktop

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/peltonapp/Pelton/internal/desktop/syncsched"
	"github.com/peltonapp/Pelton/internal/sync/pool"
)

func TestThrottleHalvesEffectiveAfterErrors(t *testing.T) {
	ctx := context.Background()
	p := pool.New(4)
	sched := syncsched.New(1)
	sched.Start(ctx, p, 0)
	t.Cleanup(sched.Stop)

	app := &App{
		ctx:   ctx,
		log:   slog.New(slog.DiscardHandler),
		syncs: map[int64]*accountSync{1: {sched: sched, pool: p, ended: make(chan struct{})}},
	}

	netErr := errors.New("dial tcp: connection refused")
	app.noteSyncPoolOutcome(1, netErr)
	if got := p.Effective(); got != 4 {
		t.Fatalf("after one throttle error effective=%d, want 4", got)
	}
	app.noteSyncPoolOutcome(1, netErr)
	if got := p.Effective(); got != 2 {
		t.Fatalf("after two throttle errors effective=%d, want 2", got)
	}
}

// a throttled account must not open more blob downloads than it has sync
// slots, or the throttle backs off connections while downloads keep piling on.
func TestJMAPBlobParallelFollowsThrottledPool(t *testing.T) {
	ctx := context.Background()
	p := pool.New(3)
	p.SetEffective(1)
	app := &App{
		ctx:   ctx,
		syncs: map[int64]*accountSync{1: {pool: p, ended: make(chan struct{})}},
	}
	if got := app.jmapBlobDownloadParallel(1); got != 1 {
		t.Fatalf("blob parallelism %d with the pool throttled to 1, want 1", got)
	}
}
