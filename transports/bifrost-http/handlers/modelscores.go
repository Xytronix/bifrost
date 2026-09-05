package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/maximhq/bifrost/core/schemas"
)

// Independent model capability scores for /v1/models.
//
// Clients otherwise each need their own benchmark API key, and the publishers
// forbid shipping keys in client-side code. Holding one key in the gateway
// keeps it server-side, spends one request per refresh for the whole fleet
// instead of one per machine, and gives every consumer (omp, AgentsView,
// cursor, anything reading /v1/models) the same numbers.
//
// Enabled by AA_API_KEY. Unset means no score request leaves the gateway and
// the field is simply absent — never fabricated.
const (
	aaScoresURL       = "https://artificialanalysis.ai/api/v2/data/llms/models"
	aaScoresSource    = "artificialanalysis.ai"
	aaScoresTTL       = 6 * time.Hour
	aaScoresTimeout   = 15 * time.Second
	aaScoresMinRows   = 64
	aaScoresRetryWait = 5 * time.Minute
)

type modelScore struct {
	Intelligence *float64
	Coding       *float64
	TokensPerSec *float64
}

type modelScoreIndex struct {
	mu         sync.RWMutex
	byKey      map[string]modelScore
	fetchedAt  time.Time
	attempted  time.Time
	refreshing bool
}

var aaScores = &modelScoreIndex{}

// scoreKey folds a model id to the form benchmark publishers key on: bare
// name, lowercased, dots and colons as dashes ("grok-4.6" -> "grok-4-6").
func scoreKey(model string) string {
	name := strings.ToLower(strings.TrimSpace(model))
	if name == "" {
		return ""
	}
	name = strings.ReplaceAll(name, ".", "-")
	name = strings.ReplaceAll(name, ":", "-")
	name = strings.ReplaceAll(name, " ", "-")
	return name
}

// stripHostNameBadge drops a leading "<provider>-" from a model id, returning
// "" when the id carries no such badge.
func stripHostNameBadge(provider, model string) string {
	prefix := strings.ToLower(strings.TrimSpace(provider)) + "-"
	if prefix == "-" || len(model) <= len(prefix) {
		return ""
	}
	if !strings.EqualFold(model[:len(prefix)], prefix) {
		return ""
	}
	return model[len(prefix):]
}

// benchmarksForModel resolves scores through the same candidate chain the
// pricing and capability lookups walk, so aggregator and effort-suffixed ids
// ("omp-gw/anthropic/claude-opus-5", "claude-opus-5-high") reach a scored row.
// Effort-suffixed candidates are tried before the stripped base name: the
// publishers score effort tiers separately and the tier is what a caller
// actually selects.
//
// The provider is needed on top of that chain because a host that re-badges a
// vendor model with its own name ("cursor/cursor-grok-4.6") has already had
// the provider split off the id by the time enrichment runs, leaving nothing
// for stripProviderNamePrefix to match against.
func benchmarksForModel(provider, model string, alias *string) *schemas.ModelBenchmarks {
	if !aaScores.enabled() {
		return nil
	}
	aaScores.ensureFresh()
	names := modelResolutionCandidates(model)
	if badge := stripHostNameBadge(provider, model); badge != "" {
		names = append(names, modelResolutionCandidates(badge)...)
	}
	if alias != nil && *alias != "" {
		names = append(names, modelResolutionCandidates(*alias)...)
	}
	seen := map[string]bool{}
	for _, cand := range names {
		key := scoreKey(cand)
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		if score, ok := aaScores.lookup(key); ok {
			return &schemas.ModelBenchmarks{
				Intelligence:          score.Intelligence,
				Coding:                score.Coding,
				OutputTokensPerSecond: score.TokensPerSec,
				Source:                aaScoresSource,
			}
		}
	}
	return nil
}

func (idx *modelScoreIndex) enabled() bool {
	return strings.TrimSpace(os.Getenv("AA_API_KEY")) != ""
}

func (idx *modelScoreIndex) lookup(key string) (modelScore, bool) {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	score, ok := idx.byKey[key]
	return score, ok
}

// ensureFresh refreshes in the background: a list-models request must never
// wait on a third-party benchmark endpoint. The first request after boot
// therefore reports no scores, and the next one has them.
func (idx *modelScoreIndex) ensureFresh() {
	now := time.Now()
	idx.mu.Lock()
	stale := now.Sub(idx.fetchedAt) >= aaScoresTTL
	retryDue := now.Sub(idx.attempted) >= aaScoresRetryWait
	if !stale || idx.refreshing || !retryDue {
		idx.mu.Unlock()
		return
	}
	idx.refreshing = true
	idx.attempted = now
	idx.mu.Unlock()

	go func() {
		rows, err := fetchAAScores(strings.TrimSpace(os.Getenv("AA_API_KEY")))
		idx.mu.Lock()
		defer idx.mu.Unlock()
		idx.refreshing = false
		// A short payload is a stub, a truncated mirror, or an intercepted
		// transport: keep the previous snapshot rather than pin the catalog to
		// a handful of scores for the whole TTL.
		if err != nil || len(rows) < aaScoresMinRows {
			return
		}
		idx.byKey = rows
		idx.fetchedAt = time.Now()
	}()
}

type aaModelsResponse struct {
	Data []struct {
		Slug        string             `json:"slug"`
		Evaluations map[string]float64 `json:"evaluations"`
		OutputTPS   *float64           `json:"median_output_tokens_per_second"`
	} `json:"data"`
}

func fetchAAScores(apiKey string) (map[string]modelScore, error) {
	ctx, cancel := context.WithTimeout(context.Background(), aaScoresTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, aaScoresURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("x-api-key", apiKey)
	req.Header.Set("Accept", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, &scoreFetchError{status: resp.StatusCode}
	}
	var payload aaModelsResponse
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, err
	}
	rows := make(map[string]modelScore, len(payload.Data))
	for _, entry := range payload.Data {
		key := scoreKey(entry.Slug)
		if key == "" {
			continue
		}
		score := modelScore{}
		if value, ok := entry.Evaluations["artificial_analysis_intelligence_index"]; ok {
			score.Intelligence = &value
		}
		if value, ok := entry.Evaluations["artificial_analysis_coding_index"]; ok {
			score.Coding = &value
		}
		// Unmeasured speed is published as 0 rather than omitted.
		if entry.OutputTPS != nil && *entry.OutputTPS > 0 {
			value := *entry.OutputTPS
			score.TokensPerSec = &value
		}
		if score.Intelligence == nil && score.Coding == nil && score.TokensPerSec == nil {
			continue
		}
		rows[key] = score
	}
	return rows, nil
}

type scoreFetchError struct {
	status int
}

func (e *scoreFetchError) Error() string {
	return "model score fetch failed with status " + http.StatusText(e.status)
}
