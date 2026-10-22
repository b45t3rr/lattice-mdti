// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The MDTI Authors

package replay

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/b45t3rr/lattice-mdti/internal/domain"
)

func frames() []domain.Frame {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	return []domain.Frame{
		{EntityKey: "adsb:a", Domain: domain.DomainAir, Source: domain.SourceRedTeam, ObservedAt: t0, Lat: -34, Lon: -58, AltBaroFt: domain.Ptr(5000.0)},
		{EntityKey: "adsb:a", Domain: domain.DomainAir, Source: domain.SourceRedTeam, ObservedAt: t0.Add(50 * time.Millisecond), Lat: -34.001, Lon: -58},
	}
}

func TestRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	rec := NewRecorder(&buf)
	for _, f := range frames() {
		require.NoError(t, rec.Write(f))
	}
	require.NoError(t, rec.Flush())
	got, err := Read(&buf)
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.Equal(t, domain.SourceRedTeam, got[0].Source, "source kept in recordings")
	assert.Equal(t, 5000.0, *got[0].AltBaroFt)
	assert.Nil(t, got[1].AltBaroFt, "absent stays absent")
	assert.True(t, got[1].ObservedAt.Equal(frames()[1].ObservedAt))
}

func TestPlayerMaxSpeedKeepsTimestamps(t *testing.T) {
	out := make(chan domain.Frame, 2)
	require.NoError(t, (&Player{Frames: frames()}).Run(context.Background(), out))
	assert.True(t, (<-out).ObservedAt.Equal(frames()[0].ObservedAt))
}

func TestPlayerRebasePreservesSpacing(t *testing.T) {
	out := make(chan domain.Frame, 2)
	start := time.Now()
	require.NoError(t, (&Player{Frames: frames(), Speed: 1, Rebase: true}).Run(context.Background(), out))
	a, b := <-out, <-out
	assert.WithinDuration(t, start, a.ObservedAt, 20*time.Millisecond)
	assert.Equal(t, 50*time.Millisecond, b.ObservedAt.Sub(a.ObservedAt))
}

func TestReadReportsBadLine(t *testing.T) {
	_, err := Read(bytes.NewBufferString("{}\n{oops\n"))
	assert.ErrorContains(t, err, "line 2")
}
