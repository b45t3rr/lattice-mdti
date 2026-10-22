// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The MDTI Authors

package viz

import (
	"bytes"
	"image/png"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLineChartIsPNG(t *testing.T) {
	b := LineChart("t", "x", "y", []Series{{Name: "a", Color: Cyan, Y: []float64{1, 2, 3}}, {Name: "b", Color: Red, Y: []float64{3, 1, 2}}})
	img, err := png.Decode(bytes.NewReader(b))
	require.NoError(t, err)
	require.Equal(t, 640, img.Bounds().Dx())
	if p := os.Getenv("MDTI_CHART_OUT"); p != "" {
		require.NoError(t, os.WriteFile(p, b, 0o644))
	}
}
