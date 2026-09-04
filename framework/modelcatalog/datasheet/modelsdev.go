package datasheet

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/bytedance/sonic"
	bifrost "github.com/maximhq/bifrost/core"
	"github.com/maximhq/bifrost/core/schemas"
)

// DefaultModelsDevURL is the community model catalog used to fill gaps the
// upstream datasheet has not published yet. models.dev exposes three views:
// /api.json (per-provider), /models.json (provider-agnostic canonical models),
// and /catalog.json (both). We take the combined one — for +220KB over
// /api.json it adds the canonical model metadata, which gives authoritative
// context limits and modalities without having to guess which provider's copy
// of a model to believe.
const DefaultModelsDevURL = "https://models.dev/catalog.json"

// modelsDevCatalog is the /catalog.json shape.
type modelsDevCatalog struct {
	// Models is keyed "lab/model" ("zhipuai/glm-4.6v") and carries the
	// provider-agnostic truth: capabilities, modalities, context limits. No
	// cost — pricing is inherently per-provider.
	Models map[string]modelsDevModel `json:"models"`
	// Providers is the /api.json body: provider id -> models -> model id.
	Providers map[string]struct {
		Models map[string]modelsDevModel `json:"models"`
	} `json:"providers"`
}

type modelsDevModel struct {
	ID   string `json:"id"`
	Cost *struct {
		Input      *float64 `json:"input"`
		Output     *float64 `json:"output"`
		CacheRead  *float64 `json:"cache_read"`
		CacheWrite *float64 `json:"cache_write"`
	} `json:"cost"`
	Limit *struct {
		Context *int `json:"context"`
		Output  *int `json:"output"`
	} `json:"limit"`
	Modalities *struct {
		Input  []string `json:"input"`
		Output []string `json:"output"`
	} `json:"modalities"`
	// ReasoningOptions enumerates how thinking is addressed. The
	// `type: "effort"` option carries the model's COMPLETE wire effort ladder
	// (`["low","medium","high","xhigh","max"]`), which the upstream datasheet
	// only ever describes as a handful of supports_*_reasoning_effort flags
	// naming the tiers beyond low/medium/high.
	// Values is []*string because a few upstream rows carry a null tier
	// (sarvam: [null,"low","medium","high"]); a []string would fail the whole
	// catalog decode and silently drop the overlay for every model.
	ReasoningOptions []struct {
		Type   string    `json:"type"`
		Values []*string `json:"values"`
	} `json:"reasoning_options"`
}

// modelsDevData is the converted catalog: pricing/capability entries plus the
// effort ladders, which are keyed the same way but consumed separately (they
// must never reach the compat plugin's supported-parameter allowlist).
type modelsDevData struct {
	Entries map[string]Entry
	Efforts map[string][]string
}

// applyModelsDevOverlay adds models.dev entries for ids the datasheet does not
// cover to the in-memory pricing map, then rebuilds the derived view.
//
// The overlay is deliberately memory-only: third-party rows never reach the
// operator's config store, so the persisted datasheet stays exactly what they
// configured and a models.dev change can never dirty their DB. It is re-applied
// after every reload, which is why this is the single commit point for
// in-memory pricing state — it MUST run (and rebuild the view) even when the
// overlay is empty or unavailable.
//
// Caller must NOT hold s.mu.
func (s *Store) applyModelsDevOverlay(ctx context.Context) {
	data := s.modelsDevData(ctx)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.applyModelsDevDataUnsafe(data)
}

// applyModelsDevDataUnsafe merges a pre-fetched overlay into pricingData and
// rebuilds every derived view as one atomic catalog publication. Caller must
// hold s.mu for writing.
func (s *Store) applyModelsDevDataUnsafe(data modelsDevData) {
	s.modelsDevEfforts = data.Efforts
	covered := make(map[string]struct{}, len(s.pricingData))
	for _, pricing := range s.pricingData {
		covered[strings.ToLower(pricing.Model)] = struct{}{}
	}
	added := 0
	for id, entry := range data.Entries {
		if strings.TrimSpace(entry.Provider) == "" {
			continue
		}
		if _, ok := covered[strings.ToLower(id)]; ok {
			continue
		}
		pricing := convertEntryToTablePricing(id, entry)
		s.pricingData[makeKey(pricing.Model, pricing.Provider, pricing.Mode)] = pricing
		added++
	}
	s.rebuildDatasheetViewUnsafe()
	if s.logger != nil {
		s.logger.Debug("models.dev overlay added %d entries the datasheet did not cover, %d effort ladders", added, len(data.Efforts))
	}
}

// modelsDevData returns the converted catalog, or the zero value when the
// overlay is off or unavailable. A file: primary URL means a local or test
// datasheet, so we never reach out to the network behind the caller's back.
func (s *Store) modelsDevData(ctx context.Context) modelsDevData {
	s.syncCfgMu.RLock()
	primaryURL, modelsDevURL := s.url, s.modelsDevURL
	s.syncCfgMu.RUnlock()

	if modelsDevURL == "" || strings.HasPrefix(primaryURL, "file:") {
		return modelsDevData{}
	}
	data, err := cachedModelsDev(ctx, modelsDevURL)
	if err != nil {
		if s.logger != nil {
			s.logger.Warn("models.dev catalog unavailable, using last-known-good overlay when available: %v", err)
		}
		if data.Entries == nil && data.Efforts == nil {
			return modelsDevData{}
		}
	}
	return data
}

// modelsDevMemoTTL bounds how long a fetched catalog is reused. The overlay
// runs on every pricing reload and a process can build many Stores; the
// catalog changes far more slowly than that.
const modelsDevMemoTTL = time.Hour

var modelsDevMemo struct {
	mu   sync.Mutex
	url  string
	at   time.Time
	data modelsDevData
}

// cachedModelsDev returns the converted catalog, fetching it at most once per
// modelsDevMemoTTL per URL. The returned maps are shared and MUST NOT be mutated.
func cachedModelsDev(ctx context.Context, rawURL string) (modelsDevData, error) {
	modelsDevMemo.mu.Lock()
	defer modelsDevMemo.mu.Unlock()
	hasMemo := (modelsDevMemo.data.Entries != nil || modelsDevMemo.data.Efforts != nil) &&
		modelsDevMemo.url == rawURL
	if hasMemo && time.Since(modelsDevMemo.at) < modelsDevMemoTTL {
		return modelsDevMemo.data, nil
	}
	data, err := fetchModelsDev(ctx, rawURL)
	if err != nil {
		if hasMemo {
			return modelsDevMemo.data, err
		}
		return modelsDevData{}, err
	}
	modelsDevMemo.url = rawURL
	modelsDevMemo.at = time.Now()
	modelsDevMemo.data = data
	return data, nil
}

// fetchModelsDev downloads and converts the models.dev catalog.
func fetchModelsDev(ctx context.Context, rawURL string) (modelsDevData, error) {
	if err := bifrost.ValidateExternalURL(rawURL, true); err != nil {
		return modelsDevData{}, fmt.Errorf("models.dev URL validation failed: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return modelsDevData{}, fmt.Errorf("failed to create models.dev request: %w", err)
	}
	client := &http.Client{Timeout: DefaultPricingTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return modelsDevData{}, fmt.Errorf("failed to download models.dev catalog: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return modelsDevData{}, fmt.Errorf("failed to download models.dev catalog: HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return modelsDevData{}, fmt.Errorf("failed to read models.dev response: %w", err)
	}
	return parseModelsDev(data)
}

// parseModelsDev converts a /catalog.json payload into datasheet entries keyed
// by bare model id. Capabilities and limits come from the canonical
// provider-agnostic models; pricing comes from the provider views, since a
// canonical model has no single price. Entries that end up with no usable rate
// AND no context limit are dropped — they would enrich nothing.
//
// A bare /api.json payload (no "models" key) still parses: the canonical half
// is simply empty and every entry comes from the provider views.
func parseModelsDev(data []byte) (modelsDevData, error) {
	var catalog modelsDevCatalog
	if err := sonic.Unmarshal(data, &catalog); err != nil {
		return modelsDevData{}, fmt.Errorf("failed to unmarshal models.dev catalog: %w", err)
	}

	out := make(map[string]Entry, len(catalog.Models))
	efforts := make(map[string][]string, len(catalog.Models))

	// Canonical first: authoritative capabilities and limits, no provider guess.
	for canonicalID, model := range catalog.Models {
		id := bareModelID(canonicalID, model.ID)
		if id == "" {
			continue
		}
		out[id] = modelsDevEntry("", model)
		if ladder := modelsDevEffortLadder(model); len(ladder) > 0 {
			efforts[id] = ladder
		}
	}

	// Provider views supply the rates, and any model the canonical half omits.
	// Walk providers in a stable order and keep the first priced entry so a
	// sync is reproducible.
	providers := make([]string, 0, len(catalog.Providers))
	for id := range catalog.Providers {
		providers = append(providers, id)
	}
	sort.Strings(providers)

	for _, providerID := range providers {
		for modelID, model := range catalog.Providers[providerID].Models {
			id := bareModelID(modelID, model.ID)
			if id == "" {
				continue
			}
			if _, ok := efforts[id]; !ok {
				if ladder := modelsDevEffortLadder(model); len(ladder) > 0 {
					efforts[id] = ladder
				}
			}
			priced := modelsDevEntry(providerID, model)
			existing, ok := out[id]
			if !ok {
				out[id] = priced
				continue
			}
			// Canonical metadata wins; only fill in what it cannot carry.
			if existing.InputCostPerToken == nil && existing.OutputCostPerToken == nil {
				existing.Provider = priced.Provider
				existing.Options = priced.Options
			}
			if existing.MaxInputTokens == nil {
				existing.MaxInputTokens = priced.MaxInputTokens
				existing.ContextLength = priced.ContextLength
			}
			if existing.MaxOutputTokens == nil {
				existing.MaxOutputTokens = priced.MaxOutputTokens
			}
			if existing.Architecture == nil {
				existing.Architecture = priced.Architecture
			}
			out[id] = existing
		}
	}

	for id, entry := range out {
		if entry.InputCostPerToken == nil && entry.OutputCostPerToken == nil &&
			entry.MaxInputTokens == nil {
			delete(out, id)
		}
	}
	// Effort ladders survive that prune: a model with neither a rate nor a
	// context limit enriches nothing pricing-wise but its thinking scale is
	// still the only published description of the wire contract.
	return modelsDevData{Entries: out, Efforts: efforts}, nil
}

// modelsDevEffortLadder returns the model's wire effort tiers, or nil when it
// exposes no effort-addressed thinking (non-reasoning models, and reasoning
// models addressed only by a toggle or a token budget).
func modelsDevEffortLadder(model modelsDevModel) []string {
	for _, opt := range model.ReasoningOptions {
		if opt.Type != "effort" || len(opt.Values) == 0 {
			continue
		}
		ladder := make([]string, 0, len(opt.Values))
		for _, value := range opt.Values {
			if value == nil || *value == "" {
				continue
			}
			ladder = append(ladder, *value)
		}
		if len(ladder) == 0 {
			continue
		}
		return ladder
	}
	return nil
}

// bareModelID strips a canonical "lab/model" prefix. The datasheet and every
// bifrost resolution candidate use bare names.
func bareModelID(key, fallback string) string {
	id := key
	if id == "" {
		id = fallback
	}
	if i := strings.LastIndex(id, "/"); i >= 0 && i+1 < len(id) {
		id = id[i+1:]
	}
	return id
}

// modelsDevEntry converts one models.dev model. Costs are per million tokens
// upstream and per token in the datasheet.
func modelsDevEntry(providerID string, model modelsDevModel) Entry {
	entry := Entry{Provider: providerID, Mode: "chat"}
	if c := model.Cost; c != nil {
		entry.Options = Options{
			InputCostPerToken:           perTokenRate(c.Input),
			OutputCostPerToken:          perTokenRate(c.Output),
			CacheReadInputTokenCost:     perTokenRate(c.CacheRead),
			CacheCreationInputTokenCost: perTokenRate(c.CacheWrite),
		}
	}
	if model.Limit != nil {
		entry.MaxInputTokens = model.Limit.Context
		entry.ContextLength = model.Limit.Context
		entry.MaxOutputTokens = model.Limit.Output
	}
	if m := model.Modalities; m != nil && (len(m.Input) > 0 || len(m.Output) > 0) {
		entry.Architecture = &schemas.Architecture{
			InputModalities:  append([]string(nil), m.Input...),
			OutputModalities: append([]string(nil), m.Output...),
		}
	}
	return entry
}

// perTokenRate converts a models.dev per-million-token rate. A zero or absent
// rate yields nil: models.dev uses 0 for "included in a subscription", which
// must not be published as a real $0 price. Those models still resolve through
// bifrost's vendor-qualified candidate form, which finds the normally-billed
// listing of the same model.
func perTokenRate(perMillion *float64) *float64 {
	if perMillion == nil || *perMillion <= 0 {
		return nil
	}
	rate := *perMillion / 1_000_000
	return &rate
}
