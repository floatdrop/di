package di_test

// A scheduler for the concurrent driver's hooks.
//
// -race and the Go scheduler *sample* interleavings. That is why the third
// review's second defect needed a five-step ordering ending in a deadline
// expiry, and why no amount of running the driver produced it: the odds of
// the sample landing there are not something more iterations fix. What the
// package needs instead is for the ordering to be an input.
//
// So every hook the driver registers, and every operation it issues, parks at
// a scheduling point, and this releases the parked goroutines one at a time
// in an order a seed decides. Replaying a seed replays the choices.
//
// What it does not do is control what happens *between* those points. A
// goroutine released here may block inside the container -- on a mutex, on a
// step channel -- where nothing in a test can see it, and the next release
// then happens on a timer rather than in lockstep. So this is systematic
// exploration, not verification: it reaches orderings that sampling does not,
// and a seed makes one reproducible enough to shrink, but two runs of a seed
// can still differ. Every oracle in concurrent_test.go stays sound under it,
// which is what makes running the same driver under a schedule worth doing at
// all.

import (
	"fmt"
	"math/rand/v2"
	"strings"
	"sync"
	"time"
)

type waiter struct {
	what string
	turn chan struct{}
}

type scheduler struct {
	mu      sync.Mutex
	rng     *rand.Rand
	parked  map[int]*waiter
	nextID  int
	trace   []string
	arrived chan struct{}
	done    chan struct{}
	stopped sync.Once

	// grace is how long the loop waits for the system to settle before
	// releasing the next goroutine anyway. Without it a released goroutine
	// that blocks inside the container would hold up every other one for
	// ever, and the deadlock would be the harness's, not the library's.
	grace time.Duration
	// settle is how long the loop lets goroutines gather at scheduling
	// points before it chooses between them. Releasing each one the moment
	// it arrives is deterministic and pointless: with one goroutine parked
	// there is nothing to decide, and the seed decides nothing. Waiting
	// first is what gives the choice a branching factor.
	settle time.Duration
}

func newScheduler(seed uint64) *scheduler {
	s := &scheduler{
		rng:     rand.New(rand.NewPCG(seed, 0x9E3779B9)),
		parked:  map[int]*waiter{},
		arrived: make(chan struct{}, 64),
		done:    make(chan struct{}),
		grace:   2 * time.Millisecond,
		settle:  200 * time.Microsecond,
	}
	go s.loop()
	return s
}

// pause parks the caller until the scheduler picks it. A nil scheduler is the
// unscheduled driver, which is the default.
func (s *scheduler) pause(what string) {
	if s == nil {
		return
	}
	w := &waiter{what: what, turn: make(chan struct{})}
	s.mu.Lock()
	select {
	case <-s.done:
		s.mu.Unlock()
		return // the run is over; do not park anyone who arrives late
	default:
	}
	id := s.nextID
	s.nextID++
	s.parked[id] = w
	s.mu.Unlock()

	select {
	case s.arrived <- struct{}{}:
	default:
	}
	select {
	case <-w.turn:
	case <-s.done:
	}
}

func (s *scheduler) loop() {
	for {
		select {
		case <-s.done:
			return
		case <-s.arrived:
			// Let whoever else is on their way arrive too, so that there is
			// something for the seed to choose between.
			select {
			case <-s.done:
				return
			case <-time.After(s.settle):
			}
		case <-time.After(s.grace):
		}
		for { // take the rest of the arrivals; the choice is made below
			select {
			case <-s.arrived:
				continue
			default:
			}
			break
		}
		s.mu.Lock()
		if len(s.parked) > 0 {
			// Choose among the ids in order, so the seed alone decides, and
			// map iteration order does not.
			ids := make([]int, 0, len(s.parked))
			for id := range s.parked {
				ids = append(ids, id)
			}
			slicesSortInts(ids)
			pick := ids[s.rng.IntN(len(ids))]
			w := s.parked[pick]
			delete(s.parked, pick)
			s.trace = append(s.trace, w.what)
			close(w.turn)
		}
		s.mu.Unlock()
	}
}

// close releases everyone still parked and ends the loop. Nothing may be left
// blocked on a scheduler that is no longer choosing.
func (s *scheduler) close() {
	if s == nil {
		return
	}
	s.stopped.Do(func() {
		s.mu.Lock()
		close(s.done)
		for id, w := range s.parked {
			delete(s.parked, id)
			close(w.turn)
		}
		s.mu.Unlock()
	})
}

// history is the order the scheduler released things in, which is what a
// failure has to be read against.
func (s *scheduler) history() string {
	if s == nil {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.trace) == 0 {
		return ""
	}
	n := len(s.trace)
	if n > 40 {
		return fmt.Sprintf("last 40 of %d scheduling points: %s", n, strings.Join(s.trace[n-40:], " -> "))
	}
	return "scheduling points: " + strings.Join(s.trace, " -> ")
}

func slicesSortInts(v []int) {
	for i := 1; i < len(v); i++ {
		for j := i; j > 0 && v[j] < v[j-1]; j-- {
			v[j], v[j-1] = v[j-1], v[j]
		}
	}
}
