// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The MDTI Authors

package evidence

import (
	"context"
	"encoding/json"
	"io"
	"testing"

	Lattice "github.com/anduril/lattice-sdk-go/v5"
	"github.com/anduril/lattice-sdk-go/v5/option"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/b45t3rr/lattice-mdti/internal/domain"
	"github.com/b45t3rr/lattice-mdti/internal/tier2"
)

type fakeObjects struct {
	path string
	body []byte
}

func (f *fakeObjects) UploadObject(_ context.Context, p string, r io.Reader, _ ...option.RequestOption) (*Lattice.PathMetadata, error) {
	f.path = p
	f.body, _ = io.ReadAll(r)
	return &Lattice.PathMetadata{SizeBytes: int64(len(f.body))}, nil
}

func TestUpload(t *testing.T) {
	api := &fakeObjects{}
	r := tier2.Result{
		Event:      domain.AnomalyEvent{ID: "019b-1", EntityID: "mdti-adsb-e3f00d", Type: domain.AnomalyGhostTrack},
		Decision:   domain.TacticalDecision{AnomalyID: "019b-1", EntityID: "mdti-adsb-e3f00d", Disposition: domain.DispositionHostile},
		Evidence:   []byte(`{"anomaly":{"type":"GHOST_TRACK"}}`),
		RawAnswers: `{"answers":[]}`,
	}
	path, err := Upload(context.Background(), api, r)
	require.NoError(t, err)
	assert.Equal(t, "mdti/evidence/mdti-adsb-e3f00d/019b-1.json", path)
	assert.Equal(t, path, api.path)
	var rec Record
	require.NoError(t, json.Unmarshal(api.body, &rec))
	assert.Equal(t, 1, rec.SchemaVersion)
	assert.Equal(t, domain.DispositionHostile, rec.TacticalDecision.Disposition)
	assert.JSONEq(t, `{"anomaly":{"type":"GHOST_TRACK"}}`, string(rec.DecisionEvidence))
}

func TestPathSanitizes(t *testing.T) {
	assert.Equal(t, "mdti/evidence/a_b__c/x_y.json", Path("a/b..c", "x y"), "no path traversal or separators")
}
