// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The MDTI Authors

// Package bus fans frames from all RX sources out to every consumer
// (SPEC §2.3). A slow consumer never blocks the others: its frames are dropped
// and counted.
package bus

import (
	"context"
	"sync"
	"sync/atomic"

	"github.com/b45t3rr/lattice-mdti/internal/domain"
)

// Bus is a single-producer-side fan-out. Register subscribers before Run.
type Bus struct {
	in   chan domain.Frame
	mu   sync.Mutex
	subs []*Subscription
}

// Subscription is one consumer's view of the stream.
type Subscription struct {
	Name    string
	C       <-chan domain.Frame
	c       chan domain.Frame
	Dropped atomic.Uint64
}

// New creates a bus whose input buffer holds size frames.
func New(size int) *Bus { return &Bus{in: make(chan domain.Frame, size)} }

// In is the channel sources write to.
func (b *Bus) In() chan<- domain.Frame { return b.in }

// Subscribe adds a consumer with its own buffer.
func (b *Bus) Subscribe(name string, size int) *Subscription {
	c := make(chan domain.Frame, size)
	s := &Subscription{Name: name, C: c, c: c}
	b.mu.Lock()
	b.subs = append(b.subs, s)
	b.mu.Unlock()
	return s
}

// Run forwards frames until ctx is cancelled, then closes every subscription.
func (b *Bus) Run(ctx context.Context) {
	b.mu.Lock()
	subs := append([]*Subscription(nil), b.subs...)
	b.mu.Unlock()
	defer func() {
		for _, s := range subs {
			close(s.c)
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return
		case f := <-b.in:
			for _, s := range subs {
				select {
				case s.c <- f:
				default:
					s.Dropped.Add(1)
				}
			}
		}
	}
}
