package datasheet

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	configstoreTables "github.com/maximhq/bifrost/framework/configstore/tables"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// modelsDevSample mirrors /catalog.json: canonical provider-agnostic models
// (capabilities + limits, no cost) plus the per-provider views (cost).
const modelsDevSample = `{
  "models": {
    "anthropic/claude-sonnet-4-6": {
      "id": "anthropic/claude-sonnet-4-6",
      "limit": {"context": 1000000, "output": 128000},
      "modalities": {"input": ["text","image","pdf"], "output": ["text"]}
    },
    "moonshotai/k3": {
      "id": "moonshotai/k3",
      "limit": {"context": 256000}
    },
    "someone/no-limit-no-price": {"id": "someone/no-limit-no-price"}
  },
  "providers": {
    "anthropic": {
      "models": {
        "claude-sonnet-4-6": {
          "id": "claude-sonnet-4-6",
          "limit": {"context": 200000},
          "cost": {"input": 3, "output": 15, "cache_read": 0.3, "cache_write": 3.75}
        }
      }
    },
    "google": {
      "models": {
        "gemini-3.6-flash": {
          "id": "gemini-3.6-flash",
          "limit": {"context": 1048576},
          "cost": {"input": 1.5, "output": 7.5, "cache_read": 0.15}
        }
      }
    },
    "kimi-for-coding": {
      "models": {"k3": {"id": "k3", "cost": {"input": 0, "output": 0}}}
    },
    "zzz-reseller": {
      "models": {
        "claude-sonnet-4-6": {"id": "claude-sonnet-4-6", "cost": {"input": 99, "output": 99}}
      }
    }
  }
}`

func TestParseModelsDev_CanonicalMetadataWinsProviderPricingFillsIn(t *testing.T) {
	entries, err := parseModelsDev([]byte(modelsDevSample))
	require.NoError(t, err)

	sonnet, ok := entries["claude-sonnet-4-6"]
	require.True(t, ok, "canonical id must be keyed bare, without the lab prefix")

	// Rates: per-million upstream, per-token in the datasheet. Providers are
	// walked in sorted order so "anthropic" wins over "zzz-reseller".
	require.NotNil(t, sonnet.InputCostPerToken)
	assert.InDelta(t, 3.0/1e6, *sonnet.InputCostPerToken, 1e-12)
	require.NotNil(t, sonnet.CacheCreationInputTokenCost)
	assert.InDelta(t, 3.75/1e6, *sonnet.CacheCreationInputTokenCost, 1e-12)

	// The canonical entry is authoritative for limits: anthropic's provider view
	// says 200k, the canonical model says 1M, and the canonical value must hold.
	require.NotNil(t, sonnet.MaxInputTokens)
	assert.Equal(t, 1000000, *sonnet.MaxInputTokens)
	require.NotNil(t, sonnet.MaxOutputTokens)
	assert.Equal(t, 128000, *sonnet.MaxOutputTokens)
	require.NotNil(t, sonnet.Architecture)
	assert.Equal(t, []string{"text", "image", "pdf"}, sonnet.Architecture.InputModalities)

	// A model only the provider half knows still lands.
	flash, ok := entries["gemini-3.6-flash"]
	require.True(t, ok)
	assert.InDelta(t, 1.5/1e6, *flash.InputCostPerToken, 1e-12)

	// Subscription-covered: models.dev writes 0, which must not become a real
	// $0 rate — but the canonical context limit is still worth keeping.
	k3, ok := entries["k3"]
	require.True(t, ok, "zero-cost model with a known limit still enriches context")
	assert.Nil(t, k3.InputCostPerToken, "an included-in-seat 0 must not be published as a price")
	require.NotNil(t, k3.MaxInputTokens)
	assert.Equal(t, 256000, *k3.MaxInputTokens)

	// Nothing to contribute at all.
	_, hasEmpty := entries["no-limit-no-price"]
	assert.False(t, hasEmpty, "entry with neither rate nor limit must be dropped")
}

func TestParseModelsDev_AcceptsBareProviderPayload(t *testing.T) {
	// /api.json has no "models" key; every entry then comes from the providers.
	entries, err := parseModelsDev([]byte(`{"google":{"models":{"gemini-3.6-flash":{"id":"gemini-3.6-flash","cost":{"input":1.5,"output":7.5}}}}}`))
	require.NoError(t, err)
	assert.Empty(t, entries, "a provider-keyed payload has no catalog envelope, so nothing is read")
}

func newOverlayStore(t *testing.T, body string) (*Store, func()) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	s := newTestStore()
	s.url = srv.URL // non-file primary so the overlay is allowed to run
	s.modelsDevURL = srv.URL
	return s, srv.Close
}

func TestApplyModelsDevOverlay_NeverOverwritesDatasheetAndRebuildsView(t *testing.T) {
	s, closeSrv := newOverlayStore(t, modelsDevSample)
	t.Cleanup(closeSrv)

	// The datasheet already prices sonnet; the overlay must leave it alone.
	s.pricingData[makeKey("claude-sonnet-4-6", "anthropic", "chat")] =
		configstoreTables.TableModelPricing{
			Model: "claude-sonnet-4-6", Provider: "anthropic", Mode: "chat",
		}

	s.applyModelsDevOverlay(context.Background())

	kept := s.pricingData[makeKey("claude-sonnet-4-6", "anthropic", "chat")]
	assert.Nil(t, kept.InputCostPerToken, "datasheet row must survive untouched")

	added := s.pricingData[makeKey("gemini-3.6-flash", "google", "chat")]
	require.NotNil(t, added.InputCostPerToken, "uncovered model must be filled in")
	assert.InDelta(t, 1.5/1e6, *added.InputCostPerToken, 1e-12)

	// The overlay is the single commit point, so the derived view must exist.
	assert.NotEmpty(t, s.datasheetByProvider, "overlay must rebuild the derived view")
}

func TestApplyModelsDevOverlay_RebuildsViewWhenUnavailable(t *testing.T) {
	// Even with the overlay off, this is still the commit point for in-memory
	// pricing state — skipping the rebuild would leave the view stale.
	s := newTestStore()
	s.url = "https://example.test/datasheet"
	s.modelsDevURL = "" // disabled
	s.pricingData[makeKey("gpt-4o", "openai", "chat")] =
		configstoreTables.TableModelPricing{Model: "gpt-4o", Provider: "openai", Mode: "chat"}

	s.applyModelsDevOverlay(context.Background())

	assert.Len(t, s.pricingData, 1)
	assert.NotEmpty(t, s.datasheetByProvider, "view rebuilt even with no overlay")
}

func TestApplyModelsDevOverlay_SkippedForLocalDatasheet(t *testing.T) {
	s := newTestStore()
	s.url = "file:///tmp/datasheet.json"
	s.modelsDevURL = "http://127.0.0.1:1/should-never-be-called"

	s.applyModelsDevOverlay(context.Background())
	assert.Empty(t, s.pricingData)
}

func TestApplyModelsDevOverlay_UpstreamFailureIsNonFatal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	s := newTestStore()
	s.url = srv.URL
	s.modelsDevURL = srv.URL
	s.pricingData[makeKey("gpt-4o", "openai", "chat")] =
		configstoreTables.TableModelPricing{Model: "gpt-4o", Provider: "openai", Mode: "chat"}

	s.applyModelsDevOverlay(context.Background())
	assert.Len(t, s.pricingData, 1, "datasheet state survives a models.dev outage")
}

func TestConfigResolved_ModelsDevToggle(t *testing.T) {
	assert.Equal(t, DefaultModelsDevURL, Config{}.resolved().ModelsDevURL)
	assert.Equal(t, "", Config{ModelsDevURL: ModelsDevDisabled}.resolved().ModelsDevURL)
	assert.Equal(t, "https://example.test/catalog.json",
		Config{ModelsDevURL: "https://example.test/catalog.json"}.resolved().ModelsDevURL)
}
