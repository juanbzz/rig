package main

import (
	"context"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/matryer/is"
)

func TestHandleSignals_FirstCancelsSecondQuits(t *testing.T) {
	i := is.New(t)

	sigs := make(chan os.Signal, 1)
	done := make(chan struct{})
	quit := make(chan os.Signal, 1)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	finished := make(chan struct{})
	go func() {
		handleSignals(sigs, cancel, done, func(sig os.Signal) { quit <- sig })
		close(finished)
	}()

	sigs <- syscall.SIGINT

	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("first signal did not cancel")
	}

	sigs <- syscall.SIGTERM

	select {
	case sig := <-quit:
		i.Equal(sig, syscall.SIGTERM)
	case <-time.After(time.Second):
		t.Fatal("second signal did not quit")
	}

	<-finished
}

func TestHandleSignals_ReturnsWhenDone(t *testing.T) {
	for name, signals := range map[string]int{"before any signal": 0, "after the first": 1} {
		t.Run(name, func(t *testing.T) {
			sigs := make(chan os.Signal, 1)
			done := make(chan struct{})

			_, cancel := context.WithCancel(context.Background())
			defer cancel()

			finished := make(chan struct{})
			go func() {
				handleSignals(sigs, cancel, done, func(os.Signal) { t.Error("quit called") })
				close(finished)
			}()

			if signals > 0 {
				sigs <- syscall.SIGINT
			}

			close(done)

			select {
			case <-finished:
			case <-time.After(time.Second):
				t.Fatal("did not return")
			}
		})
	}
}
