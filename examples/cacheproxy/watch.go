package main

import (
	"context"
	"time"
)

// Each request starts a watcher that notices when the client goes away, so
// in-flight origin work for it can be abandoned.

const watchEvery = 15 * time.Millisecond

func (a *app) startClientWatch() (stop func()) {
	a.watchClients.Store(true)
	return func() { a.watchClients.Store(false) }
}

// watchClient is called on every request after the cache lookup.
func (a *app) watchClient(ctx context.Context, hit bool) {
	if !a.watchClients.Load() {
		return
	}
	var done <-chan struct{}
	if !hit {
		done = ctx.Done()
	}
	rec := make([]byte, 512)
	go func() {
		t := time.NewTicker(watchEvery)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-a.quit:
				return
			case <-t.C:
				if ctx.Err() != nil {
					rec[0]++
				}
			}
		}
	}()
}

// endWatchers ends every watcher, leaked or not. Tests only.
func (a *app) endWatchers() {
	select {
	case <-a.quit:
	default:
		close(a.quit)
	}
}
