package clirunner

import (
	"sync"
	"time"
)

// cancellationStop owns only the supervisor's termination request. Durable
// cleanup belongs to the command's Wait owner and must never run on its timer.
type cancellationStop struct {
	mu        sync.Mutex
	terminate func()
	timer     *time.Timer
	requested bool
	started   bool
	sealed    bool
	done      chan struct{}
}

func newCancellationStop(terminate func()) *cancellationStop {
	return &cancellationStop{terminate: terminate, done: make(chan struct{})}
}

// arm admits one interruption, unless teardown has already sealed admission.
func (s *cancellationStop) arm(grace time.Duration) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sealed || s.requested {
		return false
	}
	s.requested = true
	s.timer = time.AfterFunc(grace, s.stop)
	return true
}

func (s *cancellationStop) stop() {
	s.mu.Lock()
	if s.sealed || s.started {
		s.mu.Unlock()
		return
	}
	s.started = true
	s.mu.Unlock()
	defer close(s.done)
	s.terminate()
}

// finish seals admission before joining an admitted callback. Timer.Stop alone
// does not join AfterFunc, including a callback waiting to acquire the mutex.
func (s *cancellationStop) finish() {
	s.mu.Lock()
	s.sealed = true
	if s.timer != nil {
		s.timer.Stop()
	}
	started := s.started
	s.mu.Unlock()
	if started {
		<-s.done
	}
}
