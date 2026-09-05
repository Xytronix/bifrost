package handlers

import (
	"encoding/json"
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

func TestScoreRowsFromPayload_ParsesSupportedShape(t *testing.T) {
	// Verbatim shape of the supported endpoint: speed nested under
	// "performance", "not measured" published as 0, agentic index present.
	var payload aaModelsResponse
	if err := json.Unmarshal([]byte(`{"data":[
		{"slug":"unmeasured","evaluations":{"artificial_analysis_intelligence_index":47,"artificial_analysis_agentic_index":30},"performance":{"median_output_tokens_per_second":0}},
		{"slug":"measured","evaluations":{"artificial_analysis_intelligence_index":50,"artificial_analysis_coding_index":41},"performance":{"median_output_tokens_per_second":120.5}},
		{"slug":"legacy-flat","evaluations":{"artificial_analysis_intelligence_index":51},"median_output_tokens_per_second":61.01}
	]}`), &payload); err != nil {
		t.Fatalf("payload must parse: %v", err)
	}

	rows := scoreRowsFromPayload(&payload)
	if got := rows["unmeasured"]; got.TokensPerSec != nil {
		t.Fatalf("zero speed must be dropped, got %v", *got.TokensPerSec)
	}
	if got := rows["unmeasured"]; got.Agentic == nil || *got.Agentic != 30 {
		t.Fatalf("agentic index must be read, got %#v", got.Agentic)
	}
	if got := rows["measured"]; got.TokensPerSec == nil || *got.TokensPerSec != 120.5 {
		t.Fatalf("nested speed must be read, got %#v", got.TokensPerSec)
	}
	if got := rows["measured"]; got.Coding == nil || *got.Coding != 41 {
		t.Fatalf("coding index must be read, got %#v", got.Coding)
	}
	if got := rows["legacy-flat"]; got.TokensPerSec == nil || *got.TokensPerSec != 61.01 {
		t.Fatalf("flat speed must still be read, got %#v", got.TokensPerSec)
	}
}

func TestMorePages_WalksEveryPageThenStops(t *testing.T) {
	payload := &aaModelsResponse{}
	payload.Pagination = &struct {
		Page       int  `json:"page"`
		TotalPages int  `json:"total_pages"`
		HasMore    bool `json:"has_more"`
	}{Page: 1, TotalPages: 4, HasMore: true}

	// The supported endpoints page at 200 rows: stopping at page 1 silently
	// drops three quarters of the scored catalog.
	for page := 2; page <= 4; page++ {
		if !morePages(payload, page) {
			t.Fatalf("page %d must be fetched", page)
		}
	}
	if morePages(payload, 5) {
		t.Fatalf("must not walk past total_pages")
	}
	if morePages(&aaModelsResponse{}, 2) {
		t.Fatalf("an unpaginated payload must not request a second page")
	}
}
