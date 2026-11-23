package source

import "sync/atomic"

// atomicCounter counts bytes fetched by concurrent workers.
type atomicCounter struct {
	value atomic.Int64
}

func (counter *atomicCounter) add(delta int64) { counter.value.Add(delta) }
func (counter *atomicCounter) load() int64     { return counter.value.Load() }
