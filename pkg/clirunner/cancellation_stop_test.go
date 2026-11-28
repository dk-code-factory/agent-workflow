package clirunner

import (
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

func TestCancellationStopFinishJoinsRunningTermination(t *testing.T) {
	for _, timer := range []bool{false, true} {
		name := "immediate"
		if timer {
			name = "timer"
		}
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				release := make(chan struct{})
				unblock := sync.OnceFunc(func() { close(release) })
				defer unblock()
				calls := 0
				stop := newCancellationStop(func() { calls++; <-release })
				if timer {
					stop.arm(5 * time.Second)
					time.Sleep(5 * time.Second)
				} else {
					go stop.stop()
				}
				synctest.Wait()
				if calls != 1 {
					t.Fatalf("termination did not start: %d calls", calls)
				}
				finished := make(chan struct{})
				go func() { stop.finish(); close(finished) }()
				synctest.Wait()
				select {
				case <-finished:
					t.Fatal("finish returned while termination was running")
				default:
				}
				if stop.arm(0) {
					t.Fatal("finish admitted a late cancellation")
				}
				stop.stop()
				unblock()
				synctest.Wait()
				<-finished
				stop.finish()
				if calls != 1 {
					t.Fatalf("termination repeated: %d calls", calls)
				}
			})
		})
	}
}

func TestCancellationStopFinishSealsPendingAndLateTimers(t *testing.T) {
	for _, armed := range []bool{false, true} {
		t.Run(map[bool]string{false: "before-interrupt", true: "pending-timer"}[armed], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				calls := 0
				stop := newCancellationStop(func() { calls++ })
				if armed {
					stop.arm(5 * time.Second)
				}
				stop.finish()
				if stop.arm(0) {
					t.Fatal("late interruption admitted after finish")
				}
				go stop.stop()
				time.Sleep(10 * time.Second)
				synctest.Wait()
				if calls != 0 {
					t.Fatalf("termination ran after finish: %d calls", calls)
				}
			})
		})
	}
}
