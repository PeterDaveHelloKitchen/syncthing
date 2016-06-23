// Copyright (C) 2016 The Protocol Authors.

package protocol

import "sync"

type bufferPool struct {
	minSize int
	pool    sync.Pool
}

func newBufferPool() *bufferPool {
	return &bufferPool{}
}

// get returns a new buffer of the requested size
func (p *bufferPool) get(size int) []byte {
	intf := p.pool.Get()
	if intf == nil {
		return p.new(size)
	}

	bs := intf.([]byte)
	if cap(bs) < size {
		return p.new(size)
	}

	return bs[:size]
}

// upgrade grows the buffer to the requested size, while attempting to reuse
// it if possible.
func (p *bufferPool) upgrade(bs []byte, size int) []byte {
	if cap(bs) >= size {
		return bs[:size]
	}
	p.put(bs)
	return p.get(size)
}

// put returns the buffer to the pool
func (p *bufferPool) put(bs []byte) {
	p.pool.Put(bs)
}

// new creates a new buffer of the requested size, taking the minimum
// allocation count into account. For internal use only.
func (p *bufferPool) new(size int) []byte {
	allocSize := size
	if allocSize < p.minSize {
		allocSize = p.minSize
	}
	return make([]byte, allocSize)[:size]
}
