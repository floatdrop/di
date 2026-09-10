// Optional dependencies: a key that a deployment may or may not have. A nil
// default and a null object keep every declared edge provided, so the graph
// still checks; Maybe answers presence when a constructor has to branch on a
// key nothing registered at all.
package main

import (
	"fmt"

	"github.com/floatdrop/di"
)

type Cache struct{ addr string }
type Tracer struct{}
type Store struct{ cache *Cache }
type Report struct{ line string }

type Metrics interface{ Count(string) }

type nopMetrics struct{}
type logMetrics struct{}

func (nopMetrics) Count(string)   {}
func (logMetrics) Count(n string) { fmt.Println("count:", n) }

// The constructors know nothing about di. A dependency that may be absent is
// a parameter like any other, and the nil is the absence.
func NewCache() *Cache          { return &Cache{addr: "localhost:6379"} }
func NewNopMetrics() nopMetrics { return nopMetrics{} }
func NewLogMetrics() logMetrics { return logMetrics{} }

func NewStore(c *Cache, m Metrics) *Store {
	m.Count("store.built")
	return &Store{cache: c}
}

// Something has to handle the absence, and this is where it happens.
func (s *Store) Get(key string) string {
	if s.cache == nil {
		return "db:" + key
	}
	return "cache:" + key
}

// A closure is what can ask whether a key is registered at all: Maybe is
// (T, bool), and false means nothing in this scope or above provides it.
func NewReport(s *di.Scope) *Report {
	line := s.Get[*Store]().Get("1")
	if _, ok := s.Maybe[*Tracer](); ok {
		line = "traced(" + line + ")"
	}
	return &Report{line: line}
}

// Base is the wiring every deployment shares. *Cache is provided as nil and
// Metrics as a null object, so *Store has no unprovided dependency and needs
// no di import to say it can do without them.
func Base(s *di.Scope) {
	s.Value[*Cache](nil)
	s.Wire[Metrics](NewNopMetrics)
	s.Wire[*Store](NewStore)
	s.Provide(NewReport)
}

func main() {
	plain := di.New()
	plain.Use(Base)

	// Every declared edge is provided, so there is nothing to report. The
	// closure is unchecked, which is what asking with Maybe costs.
	v := plain.Validate()
	fmt.Println("errors:   ", v.Err())
	fmt.Println("unchecked:", v.Unchecked)
	fmt.Println("plain:    ", plain.Get[*Report]().line)

	// A deployment that has a cache and a tracer registers them. The
	// defaults are overridden, and nothing that depends on them changes.
	full := di.New()
	full.Use(Base)
	full.Wire[*Cache](NewCache).Override()
	full.Wire[Metrics](NewLogMetrics).Override()
	full.Value(&Tracer{}) // a new key in this scope, so no marker is needed
	fmt.Println("full:     ", full.Get[*Report]().line)
	fmt.Print(full.Explain[*Store]())
}
