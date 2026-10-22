// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The MDTI Authors

package bus

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/b45t3rr/lattice-mdti/internal/domain"
)

func TestFanOutAndSlowConsumerDrops(t *testing.T) {
	b := New(16)
	fast := b.Subscribe("fast", 16)
	slow := b.Subscribe("slow", 1) // never read
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { b.Run(ctx); close(done) }()

	for i := range 5 {
		b.In() <- domain.Frame{EntityKey: "adsb:" + string(rune('a'+i))}
	}
	for i := range 5 {
		select {
		case f := <-fast.C:
			assert.Equal(t, "adsb:"+string(rune('a'+i)), f.EntityKey, "order preserved")
		case <-time.After(time.Second):
			t.Fatal("fast consumer starved by slow one")
		}
	}
	cancel()
	<-done
	assert.Equal(t, uint64(4), slow.Dropped.Load())
	assert.Zero(t, fast.Dropped.Load())
	_, open := <-fast.C
	require.False(t, open, "subscriptions closed on shutdown")
}
