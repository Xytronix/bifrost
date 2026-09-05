package handlers

import (
	"testing"
	"time"

	"github.com/maximhq/bifrost/core/schemas"
)

func seedScores(t *testing.T, rows map[string]modelScore) {
	t.Setenv("AA_API_KEY", "test-key")
	aaScores.mu.Lock()
	aaScores.byKey = rows
	aaScores.fetchedAt = time.Now()
	aaScores.attempted = time.Now()
	aaScores.mu.Unlock()
	t.Cleanup(func() {
		aaScores.mu.Lock()
		aaScores.byKey = nil
		aaScores.fetchedAt = time.Time{}
		aaScores.attempted = time.Time{}
		aaScores.mu.Unlock()
	})
}

func score(intelligence, coding, tps float64) modelScore {
	return modelScore{Intelligence: &intelligence, Coding: &coding, TokensPerSec: &tps}
}

func TestBenchmarksForModel_ResolvesGatewayAndEffortDialects(t *testing.T) {
	seedScores(t, map[string]modelScore{
		"claude-opus-5":      score(54.1, 47.3, 59.1),
		"claude-opus-5-high": score(52.0, 45.1, 49.7),
		"grok-4-6":           score(50.6, 44.0, 60.4),
	})

	for _, tc := range []struct {
		name     string
		provider string
		model    string
		expectIQ float64
	}{
		{"bare id", "anthropic", "claude-opus-5", 54.1},
		{"provider-qualified gateway id", "omp-gw", "omp-gw/anthropic/claude-opus-5", 54.1},
		{"effort tier scored on its own", "cursor", "claude-opus-5-high", 52.0},
		{"dotted revision folds to the published slug", "xai", "grok-4.6", 50.6},
		{"host name badge", "cursor", "cursor-grok-4.6", 50.6},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := benchmarksForModel(tc.provider, tc.model, nil)
			if got == nil || got.Intelligence == nil {
				t.Fatalf("expected a score for %q, got %#v", tc.model, got)
			}
			if *got.Intelligence != tc.expectIQ {
				t.Fatalf("expected intelligence %v for %q, got %v", tc.expectIQ, tc.model, *got.Intelligence)
			}
			if got.Source != aaScoresSource {
				t.Fatalf("scores must carry attribution, got %q", got.Source)
			}
		})
	}

	if got := benchmarksForModel("openai", "some-unscored-model", nil); got != nil {
		t.Fatalf("an unscored model must report no benchmarks, got %#v", got)
	}
}

func TestBenchmarksForModel_DisabledWithoutKey(t *testing.T) {
	seedScores(t, map[string]modelScore{"claude-opus-5": score(54.1, 47.3, 59.1)})
	t.Setenv("AA_API_KEY", "")
	if got := benchmarksForModel("anthropic", "claude-opus-5", nil); got != nil {
		t.Fatalf("no score source may be consulted without a key, got %#v", got)
	}
}

func TestEnrichListModelsResponse_AttachesBenchmarks(t *testing.T) {
	seedScores(t, map[string]modelScore{"gpt-4o": score(41.2, 33.0, 120.5)})
	catalog := modelCatalogForPricingJSON(t, []byte(`{
		"gpt-4o": {"provider":"openai","mode":"chat","base_model":"gpt-4o","max_input_tokens":128000}
	}`))
	resp := &schemas.BifrostListModelsResponse{Data: []schemas.Model{{ID: "openai/gpt-4o"}}}

	enrichListModelsResponse(resp, catalog)

	got := resp.Data[0].Benchmarks
	if got == nil || got.Intelligence == nil || *got.Intelligence != 41.2 {
		t.Fatalf("expected enriched benchmarks, got %#v", got)
	}
	if got.Coding == nil || *got.Coding != 33.0 {
		t.Fatalf("expected the coding index to be exposed alongside, got %#v", got.Coding)
	}
	if got.OutputTokensPerSecond == nil || *got.OutputTokensPerSecond != 120.5 {
		t.Fatalf("expected output speed to be exposed, got %#v", got.OutputTokensPerSecond)
	}
}
