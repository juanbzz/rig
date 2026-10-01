package stack

import (
	"context"
	"sync"
)

// group runs functions concurrently and keeps the first error, cancelling the
// rest when one fails. The others then fail with the cancellation, which is
// noise next to the error that caused it, so only the first is kept.
type group struct {
	wg     sync.WaitGroup
	once   sync.Once
	err    error
	cancel context.CancelFunc
}

func newGroup(ctx context.Context) (*group, context.Context) {
	ctx, cancel := context.WithCancel(ctx)
	return &group{cancel: cancel}, ctx
}

func (g *group) Go(f func() error) {
	g.wg.Go(func() {
		if err := f(); err != nil {
			g.once.Do(func() {
				g.err = err
				g.cancel()
			})
		}
	})
}

func (g *group) Wait() error {
	g.wg.Wait()
	g.cancel()

	return g.err
}
