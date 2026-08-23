package handlers

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bytedance/sonic"
	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/configstore"
	configstoreTables "github.com/maximhq/bifrost/framework/configstore/tables"
	"github.com/maximhq/bifrost/framework/modelcatalog"
	"github.com/maximhq/bifrost/framework/modelcatalog/datasheet"
	governanceplugin "github.com/maximhq/bifrost/plugins/governance"
	"github.com/maximhq/bifrost/transports/bifrost-http/lib"
	"github.com/valyala/fasthttp"
)

// mockModelsManager returns stable filtered and unfiltered model lists for handler tests.
// providerKeyRef records a (provider, keyID) pair a refresh was requested for.
type providerKeyRef struct {
	provider schemas.ModelProvider
	keyID    string
}

type mockModelsManager struct {
	filtered             map[schemas.ModelProvider][]string
	unfiltered           map[schemas.ModelProvider][]string
	reloadCalls          []schemas.ModelProvider
	reloadErr            error
	refreshKeyCalls      []providerKeyRef
	refreshProviderCalls []schemas.ModelProvider
	refreshErr           error
}

func (m *mockModelsManager) ReloadProvider(_ context.Context, provider schemas.ModelProvider) (*configstoreTables.TableProvider, error) {
	m.reloadCalls = append(m.reloadCalls, provider)
	if m.reloadErr != nil {
		return nil, m.reloadErr
	}
	return nil, nil
}

func (m *mockModelsManager) RemoveProvider(_ context.Context, _ schemas.ModelProvider) error {
	return nil
}

func (m *mockModelsManager) GetModelsForProvider(provider schemas.ModelProvider) []string {
	models := m.filtered[provider]
	result := make([]string, len(models))
	copy(result, models)
	return result
}

func (m *mockModelsManager) GetUnfilteredModelsForProvider(provider schemas.ModelProvider) []string {
	models := m.unfiltered[provider]
	result := make([]string, len(models))
	copy(result, models)
	return result
}

func (m *mockModelsManager) UpsertModelPricingAttributes(_ context.Context, _ []ModelPricingAttributesEntry) error {
	return nil
}

func (m *mockModelsManager) OnKeyAdded(_ context.Context, _ schemas.ModelProvider, _ schemas.Key) error {
	return nil
}

func (m *mockModelsManager) OnKeyUpdated(_ context.Context, _ schemas.ModelProvider, _ schemas.Key) error {
	return nil
}

func (m *mockModelsManager) OnKeyDeleted(_ context.Context, _ schemas.ModelProvider, _ string) error {
	return nil
}

func (m *mockModelsManager) RefreshLiveModelsForKey(_ context.Context, provider schemas.ModelProvider, keyID string) error {
	m.refreshKeyCalls = append(m.refreshKeyCalls, providerKeyRef{provider: provider, keyID: keyID})
	return m.refreshErr
}

func (m *mockModelsManager) RefreshLiveModelsForAllKeys(_ context.Context, provider schemas.ModelProvider) error {
	m.refreshProviderCalls = append(m.refreshProviderCalls, provider)
	return m.refreshErr
}

// providerHandlerForTest builds a handler with fixed provider config and model sets.
func providerHandlerForTest(provider schemas.ModelProvider, keys []schemas.Key, filtered, unfiltered []string) *ProviderHandler {
	return &ProviderHandler{
		inMemoryStore: &lib.Config{
			Providers: map[schemas.ModelProvider]configstore.ProviderConfig{
				provider: {
					Keys: keys,
				},
			},
		},
		modelsManager: &mockModelsManager{
			filtered: map[schemas.ModelProvider][]string{
				provider: filtered,
			},
			unfiltered: map[schemas.ModelProvider][]string{
				provider: unfiltered,
			},
		},
	}
}

func TestAddProvider_ReloadsRuntimeEvenWhenModelDiscoveryIsSkipped(t *testing.T) {
	SetLogger(&mockLogger{})
	lib.SetLogger(&mockLogger{})

	modelsManager := &mockModelsManager{}
	h := &ProviderHandler{
		inMemoryStore: &lib.Config{Providers: map[schemas.ModelProvider]configstore.ProviderConfig{}},
		modelsManager: modelsManager,
	}

	body, err := sonic.Marshal(providerCreatePayload{
		Provider: "mock-openai",
		CustomProviderConfig: &schemas.CustomProviderConfig{
			BaseProviderType: schemas.OpenAI,
			IsKeyLess:        true,
		},
	})
	if err != nil {
		t.Fatalf("failed to marshal request body: %v", err)
	}

	ctx := &fasthttp.RequestCtx{}
	ctx.Request.Header.SetMethod(fasthttp.MethodPost)
	ctx.Request.SetRequestURI("/api/providers")
	ctx.Request.SetBody(body)

	h.addProvider(ctx)

	if ctx.Response.StatusCode() != fasthttp.StatusOK {
		t.Fatalf("expected 200, got %d: %s", ctx.Response.StatusCode(), string(ctx.Response.Body()))
	}
	if len(modelsManager.reloadCalls) != 1 || modelsManager.reloadCalls[0] != "mock-openai" {
		t.Fatalf("expected provider reload for mock-openai, got %#v", modelsManager.reloadCalls)
	}
	if _, exists := h.inMemoryStore.Providers["mock-openai"]; !exists {
		t.Fatalf("expected provider to be added to in-memory store")
	}
}

func TestAddProvider_ReturnsErrorWhenRuntimeReloadFails(t *testing.T) {
	SetLogger(&mockLogger{})
	lib.SetLogger(&mockLogger{})

	modelsManager := &mockModelsManager{reloadErr: context.DeadlineExceeded}
	h := &ProviderHandler{
		inMemoryStore: &lib.Config{Providers: map[schemas.ModelProvider]configstore.ProviderConfig{}},
		modelsManager: modelsManager,
	}

	body, err := sonic.Marshal(providerCreatePayload{
		Provider: "mock-openai",
		CustomProviderConfig: &schemas.CustomProviderConfig{
			BaseProviderType: schemas.OpenAI,
			IsKeyLess:        true,
		},
	})
	if err != nil {
		t.Fatalf("failed to marshal request body: %v", err)
	}

	ctx := &fasthttp.RequestCtx{}
	ctx.Request.Header.SetMethod(fasthttp.MethodPost)
	ctx.Request.SetRequestURI("/api/providers")
	ctx.Request.SetBody(body)

	h.addProvider(ctx)

	if ctx.Response.StatusCode() != fasthttp.StatusInternalServerError {
		t.Fatalf("expected 500, got %d: %s", ctx.Response.StatusCode(), string(ctx.Response.Body()))
	}
	if len(modelsManager.reloadCalls) != 1 || modelsManager.reloadCalls[0] != "mock-openai" {
		t.Fatalf("expected single provider reload for mock-openai, got %#v", modelsManager.reloadCalls)
	}
	var bifrostErr schemas.BifrostError
	if err := json.Unmarshal(ctx.Response.Body(), &bifrostErr); err != nil {
		t.Fatalf("failed to unmarshal error response: %v", err)
	}
	if bifrostErr.Error == nil || bifrostErr.Error.Message == "" {
		t.Fatalf("expected error message in response, got %#v", bifrostErr)
	}
	if bifrostErr.Error.Message != "Failed to initialize provider after add: context deadline exceeded" {
		t.Fatalf("unexpected error message: %q", bifrostErr.Error.Message)
	}
	if _, exists := h.inMemoryStore.Providers["mock-openai"]; exists {
		t.Fatalf("expected provider rollback after reload failure")
	}
}

// TestUpdateProvider_RejectsKeysInBody guards against a silent-discard regression
// where `keys` is decoded into `payload.Keys` but never written to the persisted
// `ProviderConfig`. The endpoint manages provider-level config only; key edits
// must go through PUT /api/providers/{provider}/keys/{key_id}. Without this
// guard, callers (third-party API users, older dashboard bundles, integration
// tests) get HTTP 200 with their `blacklisted_models`/`weight`/etc. silently
// dropped — and the in-memory cache is rewritten with the stale `oldConfigRaw`
// keys, causing list/per-key endpoints to diverge from the DB.
func TestUpdateProvider_RejectsKeysInBody(t *testing.T) {
	SetLogger(&mockLogger{})
	lib.SetLogger(&mockLogger{})

	existingKey := schemas.Key{
		ID:                "key-existing",
		Models:            []string{"*"},
		BlacklistedModels: []string{"gpt-3.5-turbo"},
		Weight:            0.8,
	}
	h := &ProviderHandler{
		inMemoryStore: &lib.Config{
			Providers: map[schemas.ModelProvider]configstore.ProviderConfig{
				schemas.OpenAI: {Keys: []schemas.Key{existingKey}},
			},
		},
		modelsManager: &mockModelsManager{},
	}

	body, err := sonic.Marshal(struct {
		Keys                     []schemas.Key                    `json:"keys"`
		NetworkConfig            schemas.NetworkConfig            `json:"network_config"`
		ConcurrencyAndBufferSize schemas.ConcurrencyAndBufferSize `json:"concurrency_and_buffer_size"`
	}{
		Keys: []schemas.Key{{
			ID:                "key-existing",
			Models:            []string{"*"},
			BlacklistedModels: []string{"gpt-4o", "o1-preview"},
			Weight:            0.42,
		}},
		ConcurrencyAndBufferSize: schemas.ConcurrencyAndBufferSize{
			Concurrency: 1000,
			BufferSize:  5000,
		},
	})
	if err != nil {
		t.Fatalf("failed to marshal request body: %v", err)
	}

	ctx := &fasthttp.RequestCtx{}
	ctx.Request.Header.SetMethod(fasthttp.MethodPut)
	ctx.Request.SetRequestURI("/api/providers/openai")
	ctx.Request.SetBody(body)
	ctx.SetUserValue("provider", string(schemas.OpenAI))

	h.updateProvider(ctx)

	if ctx.Response.StatusCode() != fasthttp.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", ctx.Response.StatusCode(), string(ctx.Response.Body()))
	}

	var bifrostErr schemas.BifrostError
	if err := json.Unmarshal(ctx.Response.Body(), &bifrostErr); err != nil {
		t.Fatalf("failed to unmarshal error response: %v", err)
	}
	if bifrostErr.Error == nil || bifrostErr.Error.Message == "" {
		t.Fatalf("expected error message in response, got %#v", bifrostErr)
	}
	if !strings.Contains(bifrostErr.Error.Message, "/keys") {
		t.Fatalf("expected error message to mention the /keys endpoint, got %q", bifrostErr.Error.Message)
	}

	// In-memory cache must NOT have been mutated by the rejected request.
	stored, ok := h.inMemoryStore.Providers[schemas.OpenAI]
	if !ok || len(stored.Keys) != 1 {
		t.Fatalf("expected provider to retain its single existing key, got %#v", stored)
	}
	if stored.Keys[0].Weight != 0.8 || len(stored.Keys[0].BlacklistedModels) != 1 || stored.Keys[0].BlacklistedModels[0] != "gpt-3.5-turbo" {
		t.Fatalf("expected key to be untouched (weight=0.8, blacklisted=[gpt-3.5-turbo]); got weight=%v blacklisted=%v",
			stored.Keys[0].Weight, stored.Keys[0].BlacklistedModels)
	}
}

// TestUpdateProvider_PassesThroughForEmptyOrAbsentKeys locks in the explicit
// promise that the keys-guard only rejects NON-empty `keys` arrays. A future
// refactor that accidentally tightens the guard to `payload.Keys != nil` (or
// silently strips the field with `json:",omitempty"`) would silently break
// provider-level config saves that legitimately include an empty/null `keys`
// field, so we assert the guard does NOT fire for those cases.
//
// We can't easily run the handler all the way through to a 200 here because
// `inMemoryStore.UpdateProviderConfig` requires a real *bifrost.Bifrost client
// that's out of scope for a unit test. Instead, we deliberately send
// `concurrency: 0` so the handler short-circuits with a deterministic 400
// from the concurrency validator that lives AFTER the keys-guard. The
// invariant under test is: the error we get is the concurrency error, not the
// keys-not-accepted error.
func TestUpdateProvider_PassesThroughForEmptyOrAbsentKeys(t *testing.T) {
	SetLogger(&mockLogger{})
	lib.SetLogger(&mockLogger{})

	cases := []struct {
		name string
		body string
	}{
		{
			name: "keys field omitted entirely",
			body: `{
				"network_config": {},
				"concurrency_and_buffer_size": {"concurrency": 0, "buffer_size": 0}
			}`,
		},
		{
			name: "keys explicitly null",
			body: `{
				"keys": null,
				"network_config": {},
				"concurrency_and_buffer_size": {"concurrency": 0, "buffer_size": 0}
			}`,
		},
		{
			name: "keys explicitly empty array",
			body: `{
				"keys": [],
				"network_config": {},
				"concurrency_and_buffer_size": {"concurrency": 0, "buffer_size": 0}
			}`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := &ProviderHandler{
				inMemoryStore: &lib.Config{
					Providers: map[schemas.ModelProvider]configstore.ProviderConfig{
						schemas.OpenAI: {Keys: []schemas.Key{{ID: "key-existing"}}},
					},
				},
				modelsManager: &mockModelsManager{},
			}

			ctx := &fasthttp.RequestCtx{}
			ctx.Request.Header.SetMethod(fasthttp.MethodPut)
			ctx.Request.SetRequestURI("/api/providers/openai")
			ctx.Request.SetBody([]byte(tc.body))
			ctx.SetUserValue("provider", string(schemas.OpenAI))

			h.updateProvider(ctx)

			if ctx.Response.StatusCode() != fasthttp.StatusBadRequest {
				t.Fatalf("expected 400 (from concurrency validator, NOT keys-guard), got %d: %s",
					ctx.Response.StatusCode(), string(ctx.Response.Body()))
			}

			var bifrostErr schemas.BifrostError
			if err := json.Unmarshal(ctx.Response.Body(), &bifrostErr); err != nil {
				t.Fatalf("failed to unmarshal error response: %v", err)
			}
			if bifrostErr.Error == nil {
				t.Fatalf("expected error in response, got %#v", bifrostErr)
			}
			if strings.Contains(bifrostErr.Error.Message, "keys are not accepted on this endpoint") {
				t.Fatalf("keys-guard should NOT fire for empty/absent keys, got: %s", bifrostErr.Error.Message)
			}
			if !strings.Contains(bifrostErr.Error.Message, "Concurrency") {
				t.Fatalf("expected concurrency error (proves we passed the keys-guard), got: %s", bifrostErr.Error.Message)
			}
		})
	}
}

func modelCatalogForPricingJSON(t *testing.T, pricingJSON []byte) *modelcatalog.ModelCatalog {
	t.Helper()
	pricingPath := filepath.Join(t.TempDir(), "pricing.json")
	if err := os.WriteFile(pricingPath, pricingJSON, 0o600); err != nil {
		t.Fatalf("write pricing testdata: %v", err)
	}
	ds := datasheet.New(nil, nil, datasheet.Config{URL: "file://" + pricingPath})
	if err := ds.LoadFromURLIntoMemory(t.Context()); err != nil {
		t.Fatalf("load pricing testdata: %v", err)
	}
	return modelcatalog.NewTestCatalogWithDatasheet(ds)
}

func TestListModels_UnknownKeysDoNotFilter(t *testing.T) {
	SetLogger(&mockLogger{})

	h := providerHandlerForTest(
		schemas.OpenAI,
		[]schemas.Key{{ID: "key-a"}},
		[]string{"gpt-4o", "gpt-4o-mini"},
		[]string{"gpt-4o", "gpt-4o-mini"},
	)

	ctx := &fasthttp.RequestCtx{}
	ctx.Request.Header.SetMethod("GET")
	ctx.Request.SetRequestURI("/api/models?provider=openai&keys=missing")

	h.listModels(ctx)

	if ctx.Response.StatusCode() != fasthttp.StatusOK {
		t.Fatalf("expected 200, got %d: %s", ctx.Response.StatusCode(), string(ctx.Response.Body()))
	}

	var resp ListModelsResponse
	if err := json.Unmarshal(ctx.Response.Body(), &resp); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}

	if resp.Total != 2 {
		t.Fatalf("expected total=2, got %d", resp.Total)
	}
	if len(resp.Models) != 2 {
		t.Fatalf("expected all models to be returned, got %#v", resp.Models)
	}
	for _, model := range resp.Models {
		if len(model.AccessibleByKeys) != 0 {
			t.Fatalf("expected no accessible_by_keys annotations, got %#v", resp.Models)
		}
	}
}

func TestListModels_ReturnsExactAccessibleByKeysAndSkipsDisabledKeys(t *testing.T) {
	SetLogger(&mockLogger{})

	h := providerHandlerForTest(
		schemas.OpenAI,
		[]schemas.Key{
			{ID: "key-a", Models: []string{"gpt-4o"}},
			{ID: "key-b", Models: []string{"gpt-4o", "gpt-4o-mini"}},
			{ID: "key-disabled", Enabled: new(false)},
		},
		[]string{"gpt-4o", "gpt-4o-mini"},
		[]string{"gpt-4o", "gpt-4o-mini"},
	)

	ctx := &fasthttp.RequestCtx{}
	ctx.Request.Header.SetMethod("GET")
	ctx.Request.SetRequestURI("/api/models?provider=openai&keys=key-a,key-b,key-disabled")

	h.listModels(ctx)

	if ctx.Response.StatusCode() != fasthttp.StatusOK {
		t.Fatalf("expected 200, got %d: %s", ctx.Response.StatusCode(), string(ctx.Response.Body()))
	}

	var resp ListModelsResponse
	if err := json.Unmarshal(ctx.Response.Body(), &resp); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}

	if resp.Total != 2 {
		t.Fatalf("expected total=2, got %d", resp.Total)
	}

	got := map[string][]string{}
	for _, model := range resp.Models {
		got[model.Name] = model.AccessibleByKeys
	}

	if len(got["gpt-4o"]) != 2 || got["gpt-4o"][0] != "key-a" || got["gpt-4o"][1] != "key-b" {
		t.Fatalf("expected gpt-4o to be accessible by [key-a key-b], got %#v", got["gpt-4o"])
	}
	if len(got["gpt-4o-mini"]) != 1 || got["gpt-4o-mini"][0] != "key-b" {
		t.Fatalf("expected gpt-4o-mini to be accessible by [key-b], got %#v", got["gpt-4o-mini"])
	}
}

func TestListModels_AppliesQueryAndLimitAfterFiltering(t *testing.T) {
	SetLogger(&mockLogger{})

	h := providerHandlerForTest(
		schemas.OpenAI,
		[]schemas.Key{{ID: "key-a"}},
		[]string{"gpt-4o", "gpt-4o-mini", "claude-3-5-sonnet"},
		[]string{"gpt-4o", "gpt-4o-mini", "claude-3-5-sonnet"},
	)

	ctx := &fasthttp.RequestCtx{}
	ctx.Request.Header.SetMethod("GET")
	ctx.Request.SetRequestURI("/api/models?provider=openai&query=gpt&limit=1")

	h.listModels(ctx)

	if ctx.Response.StatusCode() != fasthttp.StatusOK {
		t.Fatalf("expected 200, got %d: %s", ctx.Response.StatusCode(), string(ctx.Response.Body()))
	}

	var resp ListModelsResponse
	if err := json.Unmarshal(ctx.Response.Body(), &resp); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}

	if resp.Total != 2 {
		t.Fatalf("expected total=2 after query filtering, got %d", resp.Total)
	}
	if len(resp.Models) != 1 {
		t.Fatalf("expected limit to truncate response to 1 model, got %#v", resp.Models)
	}
	if resp.Models[0].Name != "gpt-4o" {
		t.Fatalf("expected first filtered model to be gpt-4o, got %#v", resp.Models[0])
	}
}

func TestListModels_MarksDeprecatedModelsWithoutFiltering(t *testing.T) {
	SetLogger(&mockLogger{})

	h := providerHandlerForTest(
		schemas.OpenAI,
		[]schemas.Key{{ID: "key-a"}},
		[]string{"deprecated-model", "current-model", "another-current-model"},
		[]string{"deprecated-model", "current-model", "another-current-model"},
	)

	pricingJSON := []byte(`{
		"deprecated-model": {"provider":"openai","mode":"chat","base_model":"deprecated-model","is_deprecated":true},
		"current-model": {"provider":"openai","mode":"chat","base_model":"current-model"},
		"another-current-model": {"provider":"openai","mode":"chat","base_model":"another-current-model"}
	}`)
	h.inMemoryStore.ModelCatalog = modelCatalogForPricingJSON(t, pricingJSON)

	ctx := &fasthttp.RequestCtx{}
	ctx.Request.Header.SetMethod("GET")
	ctx.Request.SetRequestURI("/api/models?provider=openai&limit=10")

	h.listModels(ctx)

	if ctx.Response.StatusCode() != fasthttp.StatusOK {
		t.Fatalf("expected 200, got %d: %s", ctx.Response.StatusCode(), string(ctx.Response.Body()))
	}

	var resp ListModelsResponse
	if err := json.Unmarshal(ctx.Response.Body(), &resp); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}

	if resp.Total != 3 {
		t.Fatalf("expected total=3 (deprecated models are not filtered), got %d", resp.Total)
	}
	var deprecated *ModelResponse
	for i := range resp.Models {
		if resp.Models[i].Name == "deprecated-model" {
			deprecated = &resp.Models[i]
		}
	}
	if deprecated == nil {
		t.Fatalf("deprecated model should still be returned, got %#v", resp.Models)
	}
	if !deprecated.IsDeprecated {
		t.Fatalf("deprecated model should carry is_deprecated=true, got %#v", *deprecated)
	}
}

func TestListBaseModels_IncludesDeprecatedPricingRows(t *testing.T) {
	SetLogger(&mockLogger{})

	h := providerHandlerForTest(
		schemas.OpenAI,
		[]schemas.Key{{ID: "key-a"}},
		nil,
		nil,
	)
	h.inMemoryStore.ModelCatalog = modelCatalogForPricingJSON(t, []byte(`{
		"deprecated-model": {"provider":"openai","mode":"chat","base_model":"deprecated-base","is_deprecated":true},
		"current-model": {"provider":"openai","mode":"chat","base_model":"current-base"}
	}`))

	ctx := &fasthttp.RequestCtx{}
	ctx.Request.Header.SetMethod("GET")
	ctx.Request.SetRequestURI("/api/models/base?limit=10")

	h.listBaseModels(ctx)

	if ctx.Response.StatusCode() != fasthttp.StatusOK {
		t.Fatalf("expected 200, got %d: %s", ctx.Response.StatusCode(), string(ctx.Response.Body()))
	}

	var resp ListBaseModelsResponse
	if err := json.Unmarshal(ctx.Response.Body(), &resp); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}
	if resp.Total != 2 || !slices.Contains(resp.Models, "current-base") || !slices.Contains(resp.Models, "deprecated-base") {
		t.Fatalf("expected both base models, got %#v", resp)
	}
}

func TestEnrichListModelsResponse_MarksDeprecatedPricingRows(t *testing.T) {
	catalog := modelCatalogForPricingJSON(t, []byte(`{
		"deprecated-model": {"provider":"openai","mode":"chat","base_model":"deprecated-model","is_deprecated":true},
		"current-model": {"provider":"openai","mode":"chat","base_model":"current-model"}
	}`))
	resp := &schemas.BifrostListModelsResponse{Data: []schemas.Model{
		{ID: "openai/deprecated-model"},
		{ID: "openai/current-model"},
		{ID: "openai/provider-deprecated", IsDeprecated: true},
	}}

	enrichListModelsResponse(resp, catalog)

	if len(resp.Data) != 3 {
		t.Fatalf("expected all models retained, got %#v", resp.Data)
	}
	byID := map[string]schemas.Model{}
	for _, m := range resp.Data {
		byID[m.ID] = m
	}
	if !byID["openai/deprecated-model"].IsDeprecated {
		t.Fatalf("catalog-deprecated model should be marked deprecated: %#v", byID["openai/deprecated-model"])
	}
	if byID["openai/current-model"].IsDeprecated {
		t.Fatalf("current model should not be marked deprecated: %#v", byID["openai/current-model"])
	}
	if !byID["openai/provider-deprecated"].IsDeprecated {
		t.Fatalf("provider-deprecated flag should be preserved: %#v", byID["openai/provider-deprecated"])
	}
}

func TestEnrichListModelsResponse_ProviderAgnosticContextAndPricingFallback(t *testing.T) {
	// "Custom Router" is a registered custom/aggregator provider (so the "/"
	// prefix is stripped to the base model name) that is deliberately absent
	// from the pricing catalog, exercising the provider-agnostic fallback for
	// both capability metadata and base-model pricing.
	schemas.RegisterKnownProvider("Custom Router")
	t.Cleanup(func() { schemas.UnregisterKnownProvider("Custom Router") })

	catalog := modelCatalogForPricingJSON(t, []byte(`{
		"gpt-4o": {"provider":"openai","mode":"chat","base_model":"gpt-4o","max_input_tokens":128000,"max_output_tokens":16384,"input_cost_per_token":0.0000025}
	}`))
	resp := &schemas.BifrostListModelsResponse{Data: []schemas.Model{
		{ID: "Custom Router/gpt-4o"},
		{ID: "openai/gpt-4o"},
	}}

	enrichListModelsResponse(resp, catalog)

	byID := map[string]schemas.Model{}
	for _, m := range resp.Data {
		byID[m.ID] = m
	}

	custom := byID["Custom Router/gpt-4o"]
	if custom.ContextLength == nil || *custom.ContextLength != 128000 {
		t.Fatalf("custom model should borrow context length 128000 via base-model fallback, got %#v", custom.ContextLength)
	}
	if custom.MaxOutputTokens == nil || *custom.MaxOutputTokens != 16384 {
		t.Fatalf("custom model should borrow max output tokens 16384 via base-model fallback, got %#v", custom.MaxOutputTokens)
	}
	if custom.Pricing == nil || custom.Pricing.Prompt == nil {
		t.Fatalf("custom model should borrow base-model pricing via fallback, got %#v", custom.Pricing)
	}
	if *custom.Pricing.Prompt != "0.0000025000" {
		t.Fatalf("custom model prompt price should match base gpt-4o (0.0000025000), got %q", *custom.Pricing.Prompt)
	}

	native := byID["openai/gpt-4o"]
	if native.ContextLength == nil || *native.ContextLength != 128000 {
		t.Fatalf("native model should have context length 128000 from provider match, got %#v", native.ContextLength)
	}
	if native.Pricing == nil {
		t.Fatalf("native provider-matched model should have pricing enriched, got nil")
	}
}

func TestEnrichListModelsResponse_StripsEffortMarkerForFallbackPricing(t *testing.T) {
	// A custom/aggregator provider serving an effort-suffixed alias
	// ("claude-fable-5-low") resolves to the base model's context AND pricing by
	// stripping the identity-preserving marker — dynamically, no per-model override.
	schemas.RegisterKnownProvider("Opera")
	t.Cleanup(func() { schemas.UnregisterKnownProvider("Opera") })

	catalog := modelCatalogForPricingJSON(t, []byte(`{
		"claude-fable-5": {"provider":"anthropic","mode":"chat","base_model":"claude-fable-5","max_input_tokens":200000,"max_output_tokens":64000,"input_cost_per_token":0.00001,"output_cost_per_token":0.00005}
	}`))
	resp := &schemas.BifrostListModelsResponse{Data: []schemas.Model{
		{ID: "Opera/claude-fable-5-low"},
	}}

	enrichListModelsResponse(resp, catalog)

	m := resp.Data[0]
	if m.ContextLength == nil || *m.ContextLength != 200000 {
		t.Fatalf("effort alias should borrow context 200000 via marker-stripped base, got %#v", m.ContextLength)
	}
	if m.Pricing == nil || m.Pricing.Prompt == nil {
		t.Fatalf("effort alias should borrow base-model pricing, got %#v", m.Pricing)
	}
	if *m.Pricing.Prompt != "0.0000100000" {
		t.Fatalf("prompt price should match claude-fable-5 (0.0000100000), got %q", *m.Pricing.Prompt)
	}
}

func TestEnrichListModelsResponse_StripsInnerOrgPrefixForFallbackPricing(t *testing.T) {
	// An NVIDIA-style provider/org/model id resolves to the aggregated base
	// entry by dropping the inner org prefix
	// ("meta/llama-3.1-70b-instruct" -> "llama-3.1-70b-instruct"). Dynamic.
	schemas.RegisterKnownProvider("NVIDIA")
	t.Cleanup(func() { schemas.UnregisterKnownProvider("NVIDIA") })

	catalog := modelCatalogForPricingJSON(t, []byte(`{
		"llama-3.1-70b-instruct": {"provider":"deepinfra","mode":"chat","base_model":"llama-3.1-70b-instruct","max_input_tokens":131072,"input_cost_per_token":0.0000004,"output_cost_per_token":0.0000004}
	}`))
	resp := &schemas.BifrostListModelsResponse{Data: []schemas.Model{
		{ID: "NVIDIA/meta/llama-3.1-70b-instruct"},
	}}
	enrichListModelsResponse(resp, catalog)
	m := resp.Data[0]
	if m.Pricing == nil || m.Pricing.Prompt == nil {
		t.Fatalf("inner-org model should borrow base-model pricing via last-segment fallback, got %#v", m.Pricing)
	}
	if *m.Pricing.Prompt != "0.0000004000" {
		t.Fatalf("prompt price should match llama-3.1-70b-instruct (0.0000004000), got %q", *m.Pricing.Prompt)
	}
}
func TestEnrichListModelsResponse_SwapsClaudeVersionFamilyForFallbackPricing(t *testing.T) {
	// Cursor and Warp spell Claude version-first ("claude-4.5-sonnet",
	// "claude-4-6-opus"); the datasheet spells it family-first
	// ("claude-sonnet-4-5"), so the two never matched and the alias stayed
	// unpriced. Resolution rewrites the spelling — dynamic, no per-model map.
	schemas.RegisterKnownProvider("omp-gw")
	schemas.RegisterKnownProvider("warp")
	t.Cleanup(func() {
		schemas.UnregisterKnownProvider("omp-gw")
		schemas.UnregisterKnownProvider("warp")
	})

	catalog := modelCatalogForPricingJSON(t, []byte(`{
		"claude-sonnet-4-5": {"provider":"anthropic","mode":"chat","base_model":"claude-sonnet-4-5","max_input_tokens":200000,"input_cost_per_token":0.000003,"output_cost_per_token":0.000015},
		"claude-opus-4-6": {"provider":"anthropic","mode":"chat","base_model":"claude-opus-4-6","max_input_tokens":200000,"input_cost_per_token":0.000005,"output_cost_per_token":0.000025}
	}`))
	resp := &schemas.BifrostListModelsResponse{Data: []schemas.Model{
		{ID: "omp-gw/cursor/claude-4.5-sonnet"},
		{ID: "warp/claude-4-6-opus-high"},
	}}

	enrichListModelsResponse(resp, catalog)

	byID := map[string]schemas.Model{}
	for _, m := range resp.Data {
		byID[m.ID] = m
	}
	dotted := byID["omp-gw/cursor/claude-4.5-sonnet"]
	if dotted.Pricing == nil || dotted.Pricing.Prompt == nil {
		t.Fatalf("version-first alias should borrow claude-sonnet-4-5 pricing, got %#v", dotted.Pricing)
	}
	if *dotted.Pricing.Prompt != "0.0000030000" {
		t.Fatalf("prompt price should match claude-sonnet-4-5, got %q", *dotted.Pricing.Prompt)
	}
	// Effort marker and version swap must compose.
	dashed := byID["warp/claude-4-6-opus-high"]
	if dashed.Pricing == nil || dashed.Pricing.Prompt == nil {
		t.Fatalf("marker+swap alias should borrow claude-opus-4-6 pricing, got %#v", dashed.Pricing)
	}
	if *dashed.Pricing.Prompt != "0.0000050000" {
		t.Fatalf("prompt price should match claude-opus-4-6, got %q", *dashed.Pricing.Prompt)
	}
}

func TestEnrichListModelsResponse_StripsResellerNamePrefixForFallbackPricing(t *testing.T) {
	// Cursor republishes third-party models under its own name
	// ("cursor/cursor-grok-4.5-high"); dropping the repeated provider prefix
	// and the effort marker resolves it to the base model.
	schemas.RegisterKnownProvider("omp-gw")
	t.Cleanup(func() { schemas.UnregisterKnownProvider("omp-gw") })

	catalog := modelCatalogForPricingJSON(t, []byte(`{
		"grok-4.5": {"provider":"xai","mode":"chat","base_model":"grok-4.5","max_input_tokens":256000,"input_cost_per_token":0.000002,"output_cost_per_token":0.000006}
	}`))
	resp := &schemas.BifrostListModelsResponse{Data: []schemas.Model{
		{ID: "omp-gw/cursor/cursor-grok-4.5-high"},
	}}

	enrichListModelsResponse(resp, catalog)

	m := resp.Data[0]
	if m.Pricing == nil || m.Pricing.Prompt == nil {
		t.Fatalf("reseller-prefixed alias should borrow grok-4.5 pricing, got %#v", m.Pricing)
	}
	if *m.Pricing.Prompt != "0.0000020000" {
		t.Fatalf("prompt price should match grok-4.5 (0.0000020000), got %q", *m.Pricing.Prompt)
	}
}

func TestModelResolutionCandidates_LeavesDistinctModelsAlone(t *testing.T) {
	// The rewrites must not collapse genuinely distinct models. A name that is
	// not version-first Claude yields no swap, and a model whose prefix merely
	// resembles its provider keeps its own identity in the candidate list.
	if got := swapClaudeVersionFamily("claude-sonnet-4-5"); got != "" {
		t.Fatalf("family-first spelling must not be swapped, got %q", got)
	}
	if got := swapClaudeVersionFamily("claude-fable-5"); got != "" {
		t.Fatalf("named (non-family) Claude must not be swapped, got %q", got)
	}
	if got := stripProviderNamePrefix("openai/gpt-4o"); got != "" {
		t.Fatalf("model without a repeated provider prefix must not be stripped, got %q", got)
	}
	cands := modelResolutionCandidates("omp-gw/cursor/gpt-5.6-luna-max-fast")
	if !slices.Contains(cands, "gpt-5.6-luna") {
		t.Fatalf("stacked serving markers should reduce to the base model, got %v", cands)
	}
	if slices.Contains(cands, "gpt-5.6") {
		t.Fatalf("distinct named variant must not collapse to gpt-5.6, got %v", cands)
	}
}
func TestEnrichListModelsResponse_VendorQualifiesSubscriptionModelForPricing(t *testing.T) {
	// A subscription reseller serves a model under its short in-house id
	// ("kimi-code/k3") while the catalog lists the vendor-qualified name
	// ("kimi-k3"). Those seats are billed at the normal rate elsewhere, so the
	// qualified entry is the right price to borrow rather than leaving the
	// model unpriced.
	schemas.RegisterKnownProvider("omp-gw")
	t.Cleanup(func() { schemas.UnregisterKnownProvider("omp-gw") })

	catalog := modelCatalogForPricingJSON(t, []byte(`{
		"kimi-k3": {"provider":"moonshot","mode":"chat","base_model":"kimi-k3","max_input_tokens":1000000,"input_cost_per_token":0.000003,"output_cost_per_token":0.000015}
	}`))
	resp := &schemas.BifrostListModelsResponse{Data: []schemas.Model{
		{ID: "omp-gw/kimi-code/k3"},
	}}

	enrichListModelsResponse(resp, catalog)

	m := resp.Data[0]
	if m.Pricing == nil || m.Pricing.Prompt == nil {
		t.Fatalf("subscription alias should borrow kimi-k3 pricing, got %#v", m.Pricing)
	}
	if *m.Pricing.Prompt != "0.0000030000" {
		t.Fatalf("prompt price should match kimi-k3 (0.0000030000), got %q", *m.Pricing.Prompt)
	}
}

func TestVendorQualifiedNames(t *testing.T) {
	got := vendorQualifiedNames("omp-gw/kimi-code/k3")
	for _, want := range []string{"kimi-code-k3", "kimi-k3"} {
		if !slices.Contains(got, want) {
			t.Fatalf("expected %q among vendor-qualified forms, got %v", want, got)
		}
	}
	// Already vendor-qualified: the strip direction handles it, so adding the
	// prefix again would only produce "cursor-cursor-...".
	if got := vendorQualifiedNames("omp-gw/cursor/cursor-grok-4.5"); slices.Contains(got, "cursor-cursor-grok-4.5") {
		t.Fatalf("must not double-qualify an already-prefixed model, got %v", got)
	}
	if got := vendorQualifiedNames("gpt-4o"); len(got) != 0 {
		t.Fatalf("unqualified id has no owning provider segment, got %v", got)
	}
}

func TestEnrichListModelsResponse_DerivesVisionInputModalities(t *testing.T) {
	// The datasheet advertises vision via supports_vision (no architecture
	// block). Enrichment must surface it as architecture.input_modalities so
	// downstream consumers (omp /switch) can flag image capability — for both a
	// native provider match and a custom/aggregator base-name fallback, while
	// text-only rows stay unset.
	schemas.RegisterKnownProvider("Custom Router")
	t.Cleanup(func() { schemas.UnregisterKnownProvider("Custom Router") })

	catalog := modelCatalogForPricingJSON(t, []byte(`{
		"gpt-4o": {"provider":"openai","mode":"chat","base_model":"gpt-4o","max_input_tokens":128000,"input_cost_per_token":0.0000025,"supports_vision":true},
		"text-only-model": {"provider":"openai","mode":"chat","base_model":"text-only-model","max_input_tokens":32000,"input_cost_per_token":0.000001}
	}`))
	resp := &schemas.BifrostListModelsResponse{Data: []schemas.Model{
		{ID: "openai/gpt-4o"},
		{ID: "Custom Router/gpt-4o"},
		{ID: "openai/text-only-model"},
	}}

	enrichListModelsResponse(resp, catalog)

	byID := map[string]schemas.Model{}
	for _, m := range resp.Data {
		byID[m.ID] = m
	}
	hasImage := func(m schemas.Model) bool {
		if m.Architecture == nil {
			return false
		}
		for _, mod := range m.Architecture.InputModalities {
			if mod == "image" {
				return true
			}
		}
		return false
	}

	if !hasImage(byID["openai/gpt-4o"]) {
		t.Fatalf("native vision model should expose image input modality, got %#v", byID["openai/gpt-4o"].Architecture)
	}
	if !hasImage(byID["Custom Router/gpt-4o"]) {
		t.Fatalf("custom vision model should borrow image input modality via base-name fallback, got %#v", byID["Custom Router/gpt-4o"].Architecture)
	}
	if hasImage(byID["openai/text-only-model"]) {
		t.Fatalf("text-only model must not advertise image input, got %#v", byID["openai/text-only-model"].Architecture)
	}
}

func TestListModels_UnfilteredIgnoresKeys(t *testing.T) {
	SetLogger(&mockLogger{})

	h := providerHandlerForTest(
		schemas.OpenAI,
		[]schemas.Key{
			{ID: "key-b", Models: []string{"gpt-4o-mini"}},
		},
		[]string{"gpt-4o"},
		[]string{"gpt-4o", "gpt-4o-mini"},
	)

	ctx := &fasthttp.RequestCtx{}
	ctx.Request.Header.SetMethod("GET")
	ctx.Request.SetRequestURI("/api/models?provider=openai&keys=key-b&unfiltered=true")

	h.listModels(ctx)

	if ctx.Response.StatusCode() != fasthttp.StatusOK {
		t.Fatalf("expected 200, got %d: %s", ctx.Response.StatusCode(), string(ctx.Response.Body()))
	}

	var resp ListModelsResponse
	if err := json.Unmarshal(ctx.Response.Body(), &resp); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}

	if resp.Total != 2 || len(resp.Models) != 2 {
		t.Fatalf("expected both unfiltered models, got %#v", resp.Models)
	}

	for _, model := range resp.Models {
		if len(model.AccessibleByKeys) != 0 {
			t.Fatalf("expected no accessible_by_keys when unfiltered bypasses key filtering, got %#v", resp.Models)
		}
	}
}

func TestListModels_UnfilteredWithoutKeysReturnsAllUnfilteredModels(t *testing.T) {
	SetLogger(&mockLogger{})

	h := providerHandlerForTest(
		schemas.OpenAI,
		[]schemas.Key{
			{ID: "key-b", Models: []string{"gpt-4o-mini"}},
		},
		[]string{"gpt-4o"},
		[]string{"gpt-4o", "gpt-4o-mini"},
	)

	ctx := &fasthttp.RequestCtx{}
	ctx.Request.Header.SetMethod("GET")
	ctx.Request.SetRequestURI("/api/models?provider=openai&unfiltered=true")

	h.listModels(ctx)

	if ctx.Response.StatusCode() != fasthttp.StatusOK {
		t.Fatalf("expected 200, got %d: %s", ctx.Response.StatusCode(), string(ctx.Response.Body()))
	}

	var resp ListModelsResponse
	if err := json.Unmarshal(ctx.Response.Body(), &resp); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}

	if resp.Total != 2 || len(resp.Models) != 2 {
		t.Fatalf("expected both unfiltered models, got %#v", resp.Models)
	}

	for _, model := range resp.Models {
		if len(model.AccessibleByKeys) != 0 {
			t.Fatalf("expected no accessible_by_keys when no key filter is requested, got %#v", resp.Models)
		}
	}
}

func TestListModelDetails_ErrorsWhenModelCatalogUnavailable(t *testing.T) {
	SetLogger(&mockLogger{})

	h := providerHandlerForTest(
		schemas.OpenAI,
		[]schemas.Key{{ID: "key-a"}},
		[]string{"gpt-4o"},
		[]string{"gpt-4o"},
	)

	ctx := &fasthttp.RequestCtx{}
	ctx.Request.Header.SetMethod("GET")
	ctx.Request.SetRequestURI("/api/models/details?provider=openai")

	h.listModelDetails(ctx)

	if ctx.Response.StatusCode() != fasthttp.StatusInternalServerError {
		t.Fatalf("expected 500, got %d: %s", ctx.Response.StatusCode(), string(ctx.Response.Body()))
	}
}

func TestListModelDetails_UnknownKeysDoNotFilter(t *testing.T) {
	SetLogger(&mockLogger{})

	h := providerHandlerForTest(
		schemas.OpenAI,
		[]schemas.Key{{ID: "key-a"}},
		[]string{"gpt-4o", "gpt-4o-mini"},
		[]string{"gpt-4o", "gpt-4o-mini"},
	)
	h.inMemoryStore.ModelCatalog = modelcatalog.NewTestCatalog(nil)

	ctx := &fasthttp.RequestCtx{}
	ctx.Request.Header.SetMethod("GET")
	ctx.Request.SetRequestURI("/api/models/details?provider=openai&keys=missing")

	h.listModelDetails(ctx)

	if ctx.Response.StatusCode() != fasthttp.StatusOK {
		t.Fatalf("expected 200, got %d: %s", ctx.Response.StatusCode(), string(ctx.Response.Body()))
	}

	var resp ListModelDetailsResponse
	if err := json.Unmarshal(ctx.Response.Body(), &resp); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}

	if resp.Total != 2 || len(resp.Models) != 2 {
		t.Fatalf("expected all models when keys are unknown, got %#v", resp.Models)
	}
}

func TestListModelDetails_SkipsUnknownKeysAndFiltersWithValid(t *testing.T) {
	SetLogger(&mockLogger{})

	h := providerHandlerForTest(
		schemas.OpenAI,
		[]schemas.Key{{ID: "key-a", Models: []string{"gpt-4o"}}},
		[]string{"gpt-4o", "gpt-4o-mini"},
		[]string{"gpt-4o", "gpt-4o-mini"},
	)
	h.inMemoryStore.ModelCatalog = modelcatalog.NewTestCatalog(nil)

	ctx := &fasthttp.RequestCtx{}
	ctx.Request.Header.SetMethod("GET")
	ctx.Request.SetRequestURI("/api/models/details?provider=openai&keys=key-a,missing")

	h.listModelDetails(ctx)

	if ctx.Response.StatusCode() != fasthttp.StatusOK {
		t.Fatalf("expected 200, got %d: %s", ctx.Response.StatusCode(), string(ctx.Response.Body()))
	}

	var resp ListModelDetailsResponse
	if err := json.Unmarshal(ctx.Response.Body(), &resp); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}

	if resp.Total != 1 || len(resp.Models) != 1 {
		t.Fatalf("expected 1 model filtered by valid key, got %#v", resp.Models)
	}
	if resp.Models[0].Name != "gpt-4o" {
		t.Fatalf("expected gpt-4o, got %s", resp.Models[0].Name)
	}
}

func TestListModelDetails_SkipsDisabledKeysAndFiltersWithValid(t *testing.T) {
	SetLogger(&mockLogger{})

	h := providerHandlerForTest(
		schemas.OpenAI,
		[]schemas.Key{
			{ID: "key-a", Models: []string{"gpt-4o"}},
			{ID: "key-disabled", Enabled: new(false)},
		},
		[]string{"gpt-4o", "gpt-4o-mini"},
		[]string{"gpt-4o", "gpt-4o-mini"},
	)
	h.inMemoryStore.ModelCatalog = modelcatalog.NewTestCatalog(nil)

	ctx := &fasthttp.RequestCtx{}
	ctx.Request.Header.SetMethod("GET")
	ctx.Request.SetRequestURI("/api/models/details?provider=openai&keys=key-a,key-disabled")

	h.listModelDetails(ctx)

	if ctx.Response.StatusCode() != fasthttp.StatusOK {
		t.Fatalf("expected 200, got %d: %s", ctx.Response.StatusCode(), string(ctx.Response.Body()))
	}

	var resp ListModelDetailsResponse
	if err := json.Unmarshal(ctx.Response.Body(), &resp); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}

	if resp.Total != 1 || len(resp.Models) != 1 {
		t.Fatalf("expected 1 model filtered by valid key, got %#v", resp.Models)
	}
	if resp.Models[0].Name != "gpt-4o" {
		t.Fatalf("expected gpt-4o, got %s", resp.Models[0].Name)
	}
}

func TestListModelDetails_UnfilteredIgnoresKeys(t *testing.T) {
	SetLogger(&mockLogger{})

	h := providerHandlerForTest(
		schemas.OpenAI,
		[]schemas.Key{
			{ID: "key-b", Models: []string{"gpt-4o-mini"}},
		},
		[]string{"gpt-4o"},
		[]string{"gpt-4o", "gpt-4o-mini"},
	)
	h.inMemoryStore.ModelCatalog = modelcatalog.NewTestCatalog(nil)

	ctx := &fasthttp.RequestCtx{}
	ctx.Request.Header.SetMethod("GET")
	ctx.Request.SetRequestURI("/api/models/details?provider=openai&keys=key-b&unfiltered=true")

	h.listModelDetails(ctx)

	if ctx.Response.StatusCode() != fasthttp.StatusOK {
		t.Fatalf("expected 200, got %d: %s", ctx.Response.StatusCode(), string(ctx.Response.Body()))
	}

	var resp ListModelDetailsResponse
	if err := json.Unmarshal(ctx.Response.Body(), &resp); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}

	if resp.Total != 2 || len(resp.Models) != 2 {
		t.Fatalf("expected all unfiltered models when unfiltered=true, got %#v", resp.Models)
	}
}

func TestListModelDetails_IncludesPricing(t *testing.T) {
	SetLogger(&mockLogger{})

	h := providerHandlerForTest(
		schemas.OpenAI,
		[]schemas.Key{{ID: "key-a"}},
		[]string{"gpt-4o"},
		[]string{"gpt-4o"},
	)
	h.inMemoryStore.ModelCatalog = modelCatalogForPricingJSON(t, []byte(`{
		"gpt-4o": {
			"provider": "openai",
			"mode": "chat",
			"input_cost_per_token": 0.0000025,
			"output_cost_per_token": 0.00001,
			"cache_creation_input_token_cost": 0.000003125,
			"cache_read_input_token_cost": 0.00000025,
			"max_input_tokens": 128000,
			"max_output_tokens": 16384
		}
	}`))

	ctx := &fasthttp.RequestCtx{}
	ctx.Request.Header.SetMethod("GET")
	ctx.Request.SetRequestURI("/api/models/details?provider=openai&limit=100")

	h.listModelDetails(ctx)

	if ctx.Response.StatusCode() != fasthttp.StatusOK {
		t.Fatalf("expected 200, got %d: %s", ctx.Response.StatusCode(), string(ctx.Response.Body()))
	}

	var resp ListModelDetailsResponse
	if err := json.Unmarshal(ctx.Response.Body(), &resp); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}

	if resp.Total != 1 || len(resp.Models) != 1 {
		t.Fatalf("expected one model, got %#v", resp.Models)
	}
	if resp.Models[0].InputCostPerToken == nil || *resp.Models[0].InputCostPerToken != 0.0000025 {
		t.Fatalf("expected input cost 0.0000025, got %#v", resp.Models[0].InputCostPerToken)
	}
	if resp.Models[0].OutputCostPerToken == nil || *resp.Models[0].OutputCostPerToken != 0.00001 {
		t.Fatalf("expected output cost 0.00001, got %#v", resp.Models[0].OutputCostPerToken)
	}
	if resp.Models[0].CacheWriteCost == nil || *resp.Models[0].CacheWriteCost != 0.000003125 {
		t.Fatalf("expected cache write cost 0.000003125, got %#v", resp.Models[0].CacheWriteCost)
	}
	if resp.Models[0].CacheReadCost == nil || *resp.Models[0].CacheReadCost != 0.00000025 {
		t.Fatalf("expected cache read cost 0.00000025, got %#v", resp.Models[0].CacheReadCost)
	}
}

// gpt4oPricingJSON is the base catalog fixture shared by the override tests.
const gpt4oPricingJSON = `{
	"gpt-4o": {
		"provider": "openai",
		"mode": "chat",
		"input_cost_per_token": 0.0000025,
		"output_cost_per_token": 0.00001
	},
	"gpt-4o-mini": {
		"provider": "openai",
		"mode": "chat",
		"input_cost_per_token": 0.0000001,
		"output_cost_per_token": 0.0000004
	}
}`

// listModelDetailsForTest issues the details request and decodes the response,
// also returning the raw body so tests can assert on omitted JSON keys.
func listModelDetailsForTest(t *testing.T, h *ProviderHandler, uri string) (ListModelDetailsResponse, string) {
	t.Helper()
	ctx := &fasthttp.RequestCtx{}
	ctx.Request.Header.SetMethod("GET")
	ctx.Request.SetRequestURI(uri)

	h.listModelDetails(ctx)

	if ctx.Response.StatusCode() != fasthttp.StatusOK {
		t.Fatalf("expected 200, got %d: %s", ctx.Response.StatusCode(), string(ctx.Response.Body()))
	}
	body := string(ctx.Response.Body())
	var resp ListModelDetailsResponse
	if err := json.Unmarshal(ctx.Response.Body(), &resp); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}
	return resp, body
}

func TestListModelDetails_AppliesGlobalOverrideWithoutMutatingBase(t *testing.T) {
	SetLogger(&mockLogger{})

	h := providerHandlerForTest(schemas.OpenAI, []schemas.Key{{ID: "key-a"}}, []string{"gpt-4o"}, []string{"gpt-4o"})
	h.inMemoryStore.ModelCatalog = modelCatalogForPricingJSON(t, []byte(gpt4oPricingJSON))
	if err := h.inMemoryStore.ModelCatalog.SetPricingOverrides([]configstoreTables.TablePricingOverride{{
		ID:               "global-1",
		Name:             "Negotiated rate",
		ScopeKind:        string(modelcatalog.ScopeKindGlobal),
		MatchType:        string(modelcatalog.MatchTypeExact),
		Pattern:          "gpt-4o",
		RequestTypes:     []schemas.RequestType{schemas.ChatCompletionRequest},
		PricingPatchJSON: `{"input_cost_per_token":0.000001}`,
	}}); err != nil {
		t.Fatalf("seed overrides: %v", err)
	}

	resp, _ := listModelDetailsForTest(t, h, "/api/models/details?provider=openai&limit=100")

	if len(resp.Models) != 1 {
		t.Fatalf("expected one model, got %#v", resp.Models)
	}
	m := resp.Models[0]
	if m.InputCostPerToken == nil || *m.InputCostPerToken != 0.0000025 {
		t.Fatalf("base input cost must stay unchanged, got %#v", m.InputCostPerToken)
	}
	if m.OverriddenPricing == nil || m.OverriddenPricing.InputCostPerToken == nil ||
		*m.OverriddenPricing.InputCostPerToken != 0.000001 {
		t.Fatalf("expected overridden input cost 0.000001, got %#v", m.OverriddenPricing)
	}
	if m.OverriddenPricing.OutputCostPerToken != nil {
		t.Fatalf("output cost was not patched, expected nil, got %#v", m.OverriddenPricing.OutputCostPerToken)
	}
	if m.AppliedOverrideID != "global-1" {
		t.Fatalf("expected applied override global-1, got %q", m.AppliedOverrideID)
	}
	if len(m.PricingOverrideIDs) != 1 || m.PricingOverrideIDs[0] != "global-1" {
		t.Fatalf("expected one referenced override, got %#v", m.PricingOverrideIDs)
	}
	summary, ok := resp.PricingOverrides["global-1"]
	if !ok {
		t.Fatalf("override index missing global-1: %#v", resp.PricingOverrides)
	}
	if summary.Name != "Negotiated rate" || summary.Pattern != "gpt-4o" {
		t.Fatalf("unexpected override summary %#v", summary)
	}
	if summary.Patch.InputCostPerToken == nil || *summary.Patch.InputCostPerToken != 0.000001 {
		t.Fatalf("expected patch in summary, got %#v", summary.Patch)
	}
}

func TestListModelDetails_OverrideIndexIsDeduplicated(t *testing.T) {
	SetLogger(&mockLogger{})

	h := providerHandlerForTest(
		schemas.OpenAI,
		[]schemas.Key{{ID: "key-a"}},
		[]string{"gpt-4o", "gpt-4o-mini"},
		[]string{"gpt-4o", "gpt-4o-mini"},
	)
	h.inMemoryStore.ModelCatalog = modelCatalogForPricingJSON(t, []byte(gpt4oPricingJSON))
	if err := h.inMemoryStore.ModelCatalog.SetPricingOverrides([]configstoreTables.TablePricingOverride{{
		ID:               "wildcard-1",
		Name:             "All GPT models",
		ScopeKind:        string(modelcatalog.ScopeKindGlobal),
		MatchType:        string(modelcatalog.MatchTypeWildcard),
		Pattern:          "gpt-*",
		RequestTypes:     []schemas.RequestType{schemas.ChatCompletionRequest},
		PricingPatchJSON: `{"input_cost_per_token":0.000002}`,
	}}); err != nil {
		t.Fatalf("seed overrides: %v", err)
	}

	resp, _ := listModelDetailsForTest(t, h, "/api/models/details?provider=openai&limit=100")

	if len(resp.Models) != 2 {
		t.Fatalf("expected two models, got %#v", resp.Models)
	}
	if len(resp.PricingOverrides) != 1 {
		t.Fatalf("expected the shared override serialized once, got %#v", resp.PricingOverrides)
	}
	for _, m := range resp.Models {
		if m.AppliedOverrideID != "wildcard-1" {
			t.Fatalf("model %s missing applied override, got %q", m.Name, m.AppliedOverrideID)
		}
	}
}

func TestListModelDetails_NoOverridesOmitsNewFields(t *testing.T) {
	SetLogger(&mockLogger{})

	h := providerHandlerForTest(schemas.OpenAI, []schemas.Key{{ID: "key-a"}}, []string{"gpt-4o"}, []string{"gpt-4o"})
	h.inMemoryStore.ModelCatalog = modelCatalogForPricingJSON(t, []byte(gpt4oPricingJSON))

	_, body := listModelDetailsForTest(t, h, "/api/models/details?provider=openai&limit=100")

	for _, key := range []string{"overridden_pricing", "applied_override_id", "pricing_override_ids", "pricing_overrides"} {
		if strings.Contains(body, key) {
			t.Fatalf("expected %q to be omitted when no overrides exist: %s", key, body)
		}
	}
}

func TestListModelDetails_PatchToSameValueIsNotMarkedOverridden(t *testing.T) {
	SetLogger(&mockLogger{})

	h := providerHandlerForTest(schemas.OpenAI, []schemas.Key{{ID: "key-a"}}, []string{"gpt-4o"}, []string{"gpt-4o"})
	h.inMemoryStore.ModelCatalog = modelCatalogForPricingJSON(t, []byte(gpt4oPricingJSON))
	if err := h.inMemoryStore.ModelCatalog.SetPricingOverrides([]configstoreTables.TablePricingOverride{{
		ID:               "noop-1",
		ScopeKind:        string(modelcatalog.ScopeKindGlobal),
		MatchType:        string(modelcatalog.MatchTypeExact),
		Pattern:          "gpt-4o",
		RequestTypes:     []schemas.RequestType{schemas.ChatCompletionRequest},
		PricingPatchJSON: `{"input_cost_per_token":0.0000025}`,
	}}); err != nil {
		t.Fatalf("seed overrides: %v", err)
	}

	resp, _ := listModelDetailsForTest(t, h, "/api/models/details?provider=openai&limit=100")

	m := resp.Models[0]
	if m.OverriddenPricing != nil {
		t.Fatalf("a patch matching the base value must not render as overridden, got %#v", m.OverriddenPricing)
	}
	if m.AppliedOverrideID != "" {
		t.Fatalf("expected no applied override id, got %q", m.AppliedOverrideID)
	}
	// The override still matched the model, so it stays listed for the sheet.
	if len(m.PricingOverrideIDs) != 1 {
		t.Fatalf("expected the override to remain listed, got %#v", m.PricingOverrideIDs)
	}
}

func TestListModelDetails_OverrideWithoutBaseCatalogRow(t *testing.T) {
	SetLogger(&mockLogger{})

	h := providerHandlerForTest(
		schemas.OpenAI,
		[]schemas.Key{{ID: "key-a"}},
		[]string{"custom-model"},
		[]string{"custom-model"},
	)
	h.inMemoryStore.ModelCatalog = modelCatalogForPricingJSON(t, []byte(gpt4oPricingJSON))
	if err := h.inMemoryStore.ModelCatalog.SetPricingOverrides([]configstoreTables.TablePricingOverride{{
		ID:               "custom-1",
		ScopeKind:        string(modelcatalog.ScopeKindGlobal),
		MatchType:        string(modelcatalog.MatchTypeExact),
		Pattern:          "custom-model",
		RequestTypes:     []schemas.RequestType{schemas.ChatCompletionRequest},
		PricingPatchJSON: `{"input_cost_per_token":0.000009}`,
	}}); err != nil {
		t.Fatalf("seed overrides: %v", err)
	}

	resp, _ := listModelDetailsForTest(t, h, "/api/models/details?provider=openai&limit=100")

	m := resp.Models[0]
	if m.InputCostPerToken != nil {
		t.Fatalf("expected no base pricing, got %#v", m.InputCostPerToken)
	}
	if m.OverriddenPricing == nil || m.OverriddenPricing.InputCostPerToken == nil ||
		*m.OverriddenPricing.InputCostPerToken != 0.000009 {
		t.Fatalf("expected override-only pricing, got %#v", m.OverriddenPricing)
	}
}

func TestListModelDetails_VirtualKeyScopedOverrideIsInformationalOnly(t *testing.T) {
	SetLogger(&mockLogger{})

	vkID := "vk-1"
	h := providerHandlerForTest(schemas.OpenAI, []schemas.Key{{ID: "key-a"}}, []string{"gpt-4o"}, []string{"gpt-4o"})
	h.inMemoryStore.ModelCatalog = modelCatalogForPricingJSON(t, []byte(gpt4oPricingJSON))
	if err := h.inMemoryStore.ModelCatalog.SetPricingOverrides([]configstoreTables.TablePricingOverride{{
		ID:               "vk-scoped",
		ScopeKind:        string(modelcatalog.ScopeKindVirtualKey),
		VirtualKeyID:     &vkID,
		MatchType:        string(modelcatalog.MatchTypeExact),
		Pattern:          "gpt-4o",
		RequestTypes:     []schemas.RequestType{schemas.ChatCompletionRequest},
		PricingPatchJSON: `{"input_cost_per_token":0.000001}`,
	}}); err != nil {
		t.Fatalf("seed overrides: %v", err)
	}

	resp, _ := listModelDetailsForTest(t, h, "/api/models/details?provider=openai&limit=100")

	m := resp.Models[0]
	if m.OverriddenPricing != nil || m.AppliedOverrideID != "" {
		t.Fatalf("virtual-key scoped overrides must not change the displayed price, got %#v", m.OverriddenPricing)
	}
	if len(m.PricingOverrideIDs) != 1 || m.PricingOverrideIDs[0] != "vk-scoped" {
		t.Fatalf("expected the override listed informationally, got %#v", m.PricingOverrideIDs)
	}
}

// Provider scope is the only non-global scope that changes the displayed price,
// so it is the only case that pins the provider argument threaded into
// GetCatalogPricingOverrides. The global-scope tests above would pass even if
// that argument were wrong. The mismatched anthropic override must not surface
// at all — not even informationally.
func TestListModelDetails_AppliesProviderScopedOverride(t *testing.T) {
	SetLogger(&mockLogger{})

	openaiID := "openai"
	anthropicID := "anthropic"
	h := providerHandlerForTest(schemas.OpenAI, []schemas.Key{{ID: "key-a"}}, []string{"gpt-4o"}, []string{"gpt-4o"})
	h.inMemoryStore.ModelCatalog = modelCatalogForPricingJSON(t, []byte(gpt4oPricingJSON))
	if err := h.inMemoryStore.ModelCatalog.SetPricingOverrides([]configstoreTables.TablePricingOverride{
		{
			ID:               "provider-openai",
			Name:             "OpenAI rate",
			ScopeKind:        string(modelcatalog.ScopeKindProvider),
			ProviderID:       &openaiID,
			MatchType:        string(modelcatalog.MatchTypeExact),
			Pattern:          "gpt-4o",
			RequestTypes:     []schemas.RequestType{schemas.ChatCompletionRequest},
			PricingPatchJSON: `{"input_cost_per_token":0.000002}`,
		},
		{
			ID:               "provider-anthropic",
			Name:             "Anthropic rate",
			ScopeKind:        string(modelcatalog.ScopeKindProvider),
			ProviderID:       &anthropicID,
			MatchType:        string(modelcatalog.MatchTypeExact),
			Pattern:          "gpt-4o",
			RequestTypes:     []schemas.RequestType{schemas.ChatCompletionRequest},
			PricingPatchJSON: `{"input_cost_per_token":0.000009}`,
		},
	}); err != nil {
		t.Fatalf("seed overrides: %v", err)
	}

	resp, _ := listModelDetailsForTest(t, h, "/api/models/details?provider=openai&limit=100")

	if len(resp.Models) != 1 {
		t.Fatalf("expected one model, got %#v", resp.Models)
	}
	m := resp.Models[0]
	if m.InputCostPerToken == nil || *m.InputCostPerToken != 0.0000025 {
		t.Fatalf("base input cost must stay unchanged, got %#v", m.InputCostPerToken)
	}
	if m.OverriddenPricing == nil || m.OverriddenPricing.InputCostPerToken == nil ||
		*m.OverriddenPricing.InputCostPerToken != 0.000002 {
		t.Fatalf("expected provider-scoped override to set input cost 0.000002, got %#v", m.OverriddenPricing)
	}
	if m.AppliedOverrideID != "provider-openai" {
		t.Fatalf("expected applied override provider-openai, got %q", m.AppliedOverrideID)
	}
	if len(m.PricingOverrideIDs) != 1 || m.PricingOverrideIDs[0] != "provider-openai" {
		t.Fatalf("anthropic-scoped override must not surface for an openai model, got %#v", m.PricingOverrideIDs)
	}
	if _, ok := resp.PricingOverrides["provider-anthropic"]; ok {
		t.Fatalf("override index must not carry the mismatched provider: %#v", resp.PricingOverrides)
	}
	summary, ok := resp.PricingOverrides["provider-openai"]
	if !ok {
		t.Fatalf("override index missing provider-openai: %#v", resp.PricingOverrides)
	}
	if summary.Name != "OpenAI rate" {
		t.Fatalf("unexpected override summary %#v", summary)
	}
}

// --- VK-based filtering tests ---

// TestParseVKValueFromRequest verifies that the VK value is extracted from each
// supported header, in priority order, and that non-VK values are ignored.
func TestParseVKValueFromRequest(t *testing.T) {
	const vk = "sk-bf-test-virtual-key"

	cases := []struct {
		name   string
		setup  func(*fasthttp.RequestCtx)
		wantVK string
	}{
		{
			name: "x-bf-vk header",
			setup: func(ctx *fasthttp.RequestCtx) {
				ctx.Request.Header.Set("x-bf-vk", vk)
			},
			wantVK: vk,
		},
		{
			name: "Authorization Bearer header",
			setup: func(ctx *fasthttp.RequestCtx) {
				ctx.Request.Header.Set("Authorization", "Bearer "+vk)
			},
			wantVK: vk,
		},
		{
			name: "x-api-key header",
			setup: func(ctx *fasthttp.RequestCtx) {
				ctx.Request.Header.Set("x-api-key", vk)
			},
			wantVK: vk,
		},
		{
			name: "x-goog-api-key header",
			setup: func(ctx *fasthttp.RequestCtx) {
				ctx.Request.Header.Set("x-goog-api-key", vk)
			},
			wantVK: vk,
		},
		{
			name:   "no header returns empty string",
			setup:  func(*fasthttp.RequestCtx) {},
			wantVK: "",
		},
		{
			name: "non-VK Bearer token returns empty string",
			setup: func(ctx *fasthttp.RequestCtx) {
				ctx.Request.Header.Set("Authorization", "Bearer regular-api-key-123")
			},
			wantVK: "",
		},
		{
			name: "x-bf-vk takes priority over Authorization",
			setup: func(ctx *fasthttp.RequestCtx) {
				ctx.Request.Header.Set("x-bf-vk", vk)
				ctx.Request.Header.Set("Authorization", "Bearer sk-bf-other")
			},
			wantVK: vk,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := &fasthttp.RequestCtx{}
			tc.setup(ctx)
			got := governanceplugin.ParseVirtualKeyFromFastHTTPRequest(ctx)
			gotValue := ""
			if got != nil {
				gotValue = *got
			}
			if gotValue != tc.wantVK {
				t.Fatalf("expected %q, got %q", tc.wantVK, gotValue)
			}
		})
	}
}

// TestListModels_VKFilterRestrictsToAllowedProviderAndModels verifies that when a
// VK filter is active, only providers listed in VKProviderConfigs are returned and
// only models passing AllowedModels are included.
func TestListModels_VKFilterRestrictsToAllowedProviderAndModels(t *testing.T) {
	SetLogger(&mockLogger{})

	// Two providers configured; VK only allows openai with specific models.
	h := &ProviderHandler{
		inMemoryStore: &lib.Config{
			Providers: map[schemas.ModelProvider]configstore.ProviderConfig{
				schemas.OpenAI:    {Keys: []schemas.Key{{ID: "key-a"}}},
				schemas.Anthropic: {Keys: []schemas.Key{{ID: "key-b"}}},
			},
		},
		modelsManager: &mockModelsManager{
			filtered: map[schemas.ModelProvider][]string{
				schemas.OpenAI:    {"gpt-4o", "gpt-4o-mini", "gpt-3.5-turbo"},
				schemas.Anthropic: {"claude-3-5-sonnet", "claude-3-haiku"},
			},
		},
	}

	query := modelListQuery{
		Limit:       100,
		HasVKFilter: true,
		VKProviderConfigs: []configstoreTables.TableVirtualKeyProviderConfig{
			{
				Provider:      "openai",
				AllowedModels: schemas.WhiteList{"gpt-4o", "gpt-4o-mini"},
			},
		},
	}

	models, total, err := h.listManagementModels(query)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if total != 2 {
		t.Fatalf("expected total=2, got %d", total)
	}
	for _, m := range models {
		if m.Provider != schemas.OpenAI {
			t.Fatalf("expected only openai models, got provider %s", m.Provider)
		}
	}
	names := map[string]bool{}
	for _, m := range models {
		names[m.Name] = true
	}
	if !names["gpt-4o"] || !names["gpt-4o-mini"] {
		t.Fatalf("expected gpt-4o and gpt-4o-mini, got %v", models)
	}
	if names["gpt-3.5-turbo"] {
		t.Fatalf("gpt-3.5-turbo should be denied by AllowedModels")
	}
	if names["claude-3-5-sonnet"] || names["claude-3-haiku"] {
		t.Fatalf("anthropic models should be excluded by VK provider filter")
	}
}

// TestListModels_VKFilterAllowsAllModelsWithWildcard verifies that AllowedModels=["*"]
// passes all provider models through.
func TestListModels_VKFilterAllowsAllModelsWithWildcard(t *testing.T) {
	SetLogger(&mockLogger{})

	h := &ProviderHandler{
		inMemoryStore: &lib.Config{
			Providers: map[schemas.ModelProvider]configstore.ProviderConfig{
				schemas.OpenAI: {Keys: []schemas.Key{{ID: "key-a"}}},
			},
		},
		modelsManager: &mockModelsManager{
			filtered: map[schemas.ModelProvider][]string{
				schemas.OpenAI: {"gpt-4o", "gpt-4o-mini", "gpt-3.5-turbo"},
			},
		},
	}

	query := modelListQuery{
		Limit:       100,
		HasVKFilter: true,
		VKProviderConfigs: []configstoreTables.TableVirtualKeyProviderConfig{
			{Provider: "openai", AllowedModels: schemas.WhiteList{"*"}},
		},
	}

	models, total, err := h.listManagementModels(query)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if total != 3 {
		t.Fatalf("expected all 3 models with wildcard, got total=%d", total)
	}
	_ = models
}

// TestListModels_VKFilterDeniesAllModelsWhenAllowedModelsEmpty verifies deny-by-default:
// a VK that lists a provider but with an empty AllowedModels returns 0 models.
func TestListModels_VKFilterDeniesAllModelsWhenAllowedModelsEmpty(t *testing.T) {
	SetLogger(&mockLogger{})

	h := &ProviderHandler{
		inMemoryStore: &lib.Config{
			Providers: map[schemas.ModelProvider]configstore.ProviderConfig{
				schemas.OpenAI: {Keys: []schemas.Key{{ID: "key-a"}}},
			},
		},
		modelsManager: &mockModelsManager{
			filtered: map[schemas.ModelProvider][]string{
				schemas.OpenAI: {"gpt-4o", "gpt-4o-mini"},
			},
		},
	}

	query := modelListQuery{
		Limit:       100,
		HasVKFilter: true,
		VKProviderConfigs: []configstoreTables.TableVirtualKeyProviderConfig{
			{Provider: "openai", AllowedModels: schemas.WhiteList{}},
		},
	}

	models, total, err := h.listManagementModels(query)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if total != 0 || len(models) != 0 {
		t.Fatalf("expected 0 models with empty AllowedModels (deny-by-default), got total=%d %v", total, models)
	}
}

// TestListModels_VKFilterNoProviderConfigsDeniesAll verifies that a VK with no
// ProviderConfigs returns 0 models (deny-by-default at provider level).
func TestListModels_VKFilterNoProviderConfigsDeniesAll(t *testing.T) {
	SetLogger(&mockLogger{})

	h := &ProviderHandler{
		inMemoryStore: &lib.Config{
			Providers: map[schemas.ModelProvider]configstore.ProviderConfig{
				schemas.OpenAI:    {},
				schemas.Anthropic: {},
			},
		},
		modelsManager: &mockModelsManager{
			filtered: map[schemas.ModelProvider][]string{
				schemas.OpenAI:    {"gpt-4o"},
				schemas.Anthropic: {"claude-3-5-sonnet"},
			},
		},
	}

	query := modelListQuery{
		Limit:             100,
		HasVKFilter:       true,
		VKProviderConfigs: []configstoreTables.TableVirtualKeyProviderConfig{}, // empty
	}

	models, total, err := h.listManagementModels(query)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if total != 0 || len(models) != 0 {
		t.Fatalf("expected 0 models when VK has no provider configs, got total=%d", total)
	}
}

func TestListModels_VKFilterBlockedExplicitProviderReturnsEmptyResult(t *testing.T) {
	SetLogger(&mockLogger{})

	h := &ProviderHandler{
		inMemoryStore: &lib.Config{
			Providers: map[schemas.ModelProvider]configstore.ProviderConfig{
				schemas.OpenAI:    {},
				schemas.Anthropic: {},
			},
		},
		modelsManager: &mockModelsManager{
			filtered: map[schemas.ModelProvider][]string{
				schemas.OpenAI:    {"gpt-4o"},
				schemas.Anthropic: {"claude-3-5-sonnet"},
			},
		},
	}

	ctx := &fasthttp.RequestCtx{}
	ctx.Request.Header.SetMethod("GET")
	ctx.Request.SetRequestURI("/api/models?provider=anthropic")
	query, ok := h.parseModelListQuery(ctx, 5)
	if !ok {
		t.Fatalf("expected parseModelListQuery to succeed")
	}
	query.HasVKFilter = true
	query.VKProviderConfigs = []configstoreTables.TableVirtualKeyProviderConfig{
		{Provider: "openai", AllowedModels: schemas.WhiteList{"*"}},
	}

	models, total, err := h.listManagementModels(query)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if total != 0 || len(models) != 0 {
		t.Fatalf("expected blocked explicit provider to return no models, got total=%d models=%#v", total, models)
	}
}

func TestParseModelListQuery_VKWithoutDBStoreReturnsServiceUnavailable(t *testing.T) {
	SetLogger(&mockLogger{})

	h := &ProviderHandler{
		inMemoryStore: &lib.Config{
			Providers: map[schemas.ModelProvider]configstore.ProviderConfig{
				schemas.OpenAI: {},
			},
		},
		modelsManager: &mockModelsManager{
			filtered: map[schemas.ModelProvider][]string{
				schemas.OpenAI: {"gpt-4o", "gpt-4o-mini"},
			},
		},
	}

	ctx := &fasthttp.RequestCtx{}
	ctx.Request.Header.SetMethod("GET")
	ctx.Request.SetRequestURI("/api/models")
	ctx.Request.Header.Set("x-bf-vk", "sk-bf-test-virtual-key")

	query, ok := h.parseModelListQuery(ctx, 5)
	if ok {
		t.Fatalf("expected parseModelListQuery to fail without dbStore, got query=%#v", query)
	}
	if ctx.Response.StatusCode() != fasthttp.StatusServiceUnavailable {
		t.Fatalf("expected 503 when dbStore is unavailable, got %d", ctx.Response.StatusCode())
	}
}

// TestListModels_NoVKFilterReturnsAll verifies that without a VK filter the endpoint
// returns all providers and models as normal.
func TestListModels_NoVKFilterReturnsAll(t *testing.T) {
	SetLogger(&mockLogger{})

	h := &ProviderHandler{
		inMemoryStore: &lib.Config{
			Providers: map[schemas.ModelProvider]configstore.ProviderConfig{
				schemas.OpenAI:    {},
				schemas.Anthropic: {},
			},
		},
		modelsManager: &mockModelsManager{
			filtered: map[schemas.ModelProvider][]string{
				schemas.OpenAI:    {"gpt-4o"},
				schemas.Anthropic: {"claude-3-5-sonnet"},
			},
		},
	}

	query := modelListQuery{
		Limit:       100,
		HasVKFilter: false, // no filter
	}

	models, total, err := h.listManagementModels(query)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if total != 2 || len(models) != 2 {
		t.Fatalf("expected 2 models (one per provider), got total=%d", total)
	}
}

func TestListModels_UsesCatalogAwareAliasMatchingForKeyAllowlist(t *testing.T) {
	SetLogger(&mockLogger{})

	h := providerHandlerForTest(
		schemas.OpenAI,
		[]schemas.Key{
			{ID: "key-a", Models: []string{"gpt-4o-2024-08-06"}},
		},
		[]string{"gpt-4o"},
		[]string{"gpt-4o"},
	)
	h.inMemoryStore.ModelCatalog = modelcatalog.NewTestCatalog(map[string]string{
		"gpt-4o-2024-08-06": "gpt-4o",
	})

	ctx := &fasthttp.RequestCtx{}
	ctx.Request.Header.SetMethod("GET")
	ctx.Request.SetRequestURI("/api/models?provider=openai&keys=key-a")

	h.listModels(ctx)

	if ctx.Response.StatusCode() != fasthttp.StatusOK {
		t.Fatalf("expected 200, got %d: %s", ctx.Response.StatusCode(), string(ctx.Response.Body()))
	}

	var resp ListModelsResponse
	if err := json.Unmarshal(ctx.Response.Body(), &resp); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}

	if resp.Total != 1 || len(resp.Models) != 1 || resp.Models[0].Name != "gpt-4o" {
		t.Fatalf("expected gpt-4o to be matched through alias allowlist, got %#v", resp.Models)
	}
}

// TestListModels_KeyModelAllowlistIsCaseInsensitive verifies that key.Models matching
// uses case-insensitive comparison so "GPT-4O" in the allowlist matches "gpt-4o" in the pool.
func TestListModels_KeyModelAllowlistIsCaseInsensitive(t *testing.T) {
	SetLogger(&mockLogger{})

	h := providerHandlerForTest(
		schemas.OpenAI,
		[]schemas.Key{
			{ID: "key-a", Models: []string{"GPT-4O", "GPT-4O-MINI"}},
		},
		[]string{"gpt-4o", "gpt-4o-mini", "gpt-3.5-turbo"},
		[]string{"gpt-4o", "gpt-4o-mini", "gpt-3.5-turbo"},
	)

	ctx := &fasthttp.RequestCtx{}
	ctx.Request.Header.SetMethod("GET")
	ctx.Request.SetRequestURI("/api/models?provider=openai&keys=key-a&limit=10")

	h.listModels(ctx)

	if ctx.Response.StatusCode() != fasthttp.StatusOK {
		t.Fatalf("expected 200, got %d: %s", ctx.Response.StatusCode(), string(ctx.Response.Body()))
	}

	var resp ListModelsResponse
	if err := json.Unmarshal(ctx.Response.Body(), &resp); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}

	if resp.Total != 2 {
		t.Fatalf("expected total=2 (gpt-4o and gpt-4o-mini matched case-insensitively), got total=%d %v", resp.Total, resp.Models)
	}
	names := map[string]bool{}
	for _, m := range resp.Models {
		names[m.Name] = true
	}
	if !names["gpt-4o"] || !names["gpt-4o-mini"] {
		t.Fatalf("expected gpt-4o and gpt-4o-mini, got %v", resp.Models)
	}
	if names["gpt-3.5-turbo"] {
		t.Fatalf("gpt-3.5-turbo should not be returned (not in key allowlist)")
	}
}

// TestListModels_KeyBlacklistIsCaseInsensitive verifies that key.BlacklistedModels uses
// case-insensitive matching so "GPT-3.5-TURBO" blocks "gpt-3.5-turbo" in the pool.
func TestListModels_KeyBlacklistIsCaseInsensitive(t *testing.T) {
	SetLogger(&mockLogger{})

	h := providerHandlerForTest(
		schemas.OpenAI,
		[]schemas.Key{
			{ID: "key-a", BlacklistedModels: []string{"GPT-3.5-TURBO"}},
		},
		[]string{"gpt-4o", "gpt-4o-mini", "gpt-3.5-turbo"},
		[]string{"gpt-4o", "gpt-4o-mini", "gpt-3.5-turbo"},
	)

	ctx := &fasthttp.RequestCtx{}
	ctx.Request.Header.SetMethod("GET")
	ctx.Request.SetRequestURI("/api/models?provider=openai&keys=key-a&limit=10")

	h.listModels(ctx)

	if ctx.Response.StatusCode() != fasthttp.StatusOK {
		t.Fatalf("expected 200, got %d: %s", ctx.Response.StatusCode(), string(ctx.Response.Body()))
	}

	var resp ListModelsResponse
	if err := json.Unmarshal(ctx.Response.Body(), &resp); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}

	if resp.Total != 2 {
		t.Fatalf("expected total=2 (gpt-3.5-turbo blocked case-insensitively), got total=%d %v", resp.Total, resp.Models)
	}
	for _, m := range resp.Models {
		if strings.EqualFold(m.Name, "gpt-3.5-turbo") {
			t.Fatalf("gpt-3.5-turbo should be blocked by blacklist, got %v", resp.Models)
		}
	}
}

func TestFallbackEntryForModel_PrefersPricedOverMetadataOnly(t *testing.T) {
	// The catalog can know a model's context window without knowing its rate
	// (models.dev publishes plenty, and a seat-included model has none). The
	// walk must not stop on that row and pin the model to $0 — it has to keep
	// going to the vendor-qualified candidate that does carry a rate, while
	// still using the metadata-only row when nothing priced exists.
	schemas.RegisterKnownProvider("omp-gw")
	t.Cleanup(func() { schemas.UnregisterKnownProvider("omp-gw") })

	catalog := modelCatalogForPricingJSON(t, []byte(`{
		"k3": {"provider":"kimi","mode":"chat","base_model":"k3","max_input_tokens":1048576},
		"kimi-k3": {"provider":"moonshot","mode":"chat","base_model":"kimi-k3","max_input_tokens":1048576,"input_cost_per_token":0.000003,"output_cost_per_token":0.000015},
		"orphan-model": {"provider":"someone","mode":"chat","base_model":"orphan-model","max_input_tokens":32000}
	}`))
	resp := &schemas.BifrostListModelsResponse{Data: []schemas.Model{
		{ID: "omp-gw/kimi-code/k3"},
		{ID: "omp-gw/kimi-code/orphan-model"},
	}}

	enrichListModelsResponse(resp, catalog)

	byID := map[string]schemas.Model{}
	for _, m := range resp.Data {
		byID[m.ID] = m
	}
	k3 := byID["omp-gw/kimi-code/k3"]
	if k3.Pricing == nil || k3.Pricing.Prompt == nil {
		t.Fatalf("must walk past the unpriced k3 row to kimi-k3, got %#v", k3.Pricing)
	}
	if *k3.Pricing.Prompt != "0.0000030000" {
		t.Fatalf("prompt price should match kimi-k3, got %q", *k3.Pricing.Prompt)
	}
	orphan := byID["omp-gw/kimi-code/orphan-model"]
	if orphan.ContextLength == nil || *orphan.ContextLength != 32000 {
		t.Fatalf("metadata-only row must still enrich context, got %#v", orphan.ContextLength)
	}
}

func TestRefreshListModels_ProviderlessKeyCachesEmptyCatalog(t *testing.T) {
	// An MCP-only virtual key grants tool access and zero providers. Its model
	// catalog is legitimately empty, but the fan-out errors and leaves the
	// cache nil, which listModelsCached reports as an opaque 400
	// "failed to list models". Record the empty result instead.
	key := "vk-mcp-only:*:false"
	listAllModelsCacheMu.Lock()
	delete(listAllModelsCache, key)
	listAllModelsCacheMu.Unlock()
	t.Cleanup(func() {
		listAllModelsCacheMu.Lock()
		delete(listAllModelsCache, key)
		listAllModelsCacheMu.Unlock()
	})

	// The guard runs before the client check, so a nil client is fine here.
	h := &CompletionHandler{}
	done := make(chan struct{})
	h.refreshListModels(key, "vk-mcp-only", []schemas.ModelProvider{}, true, "", false, done)
	<-done

	listAllModelsCacheMu.Lock()
	entry := listAllModelsCache[key]
	listAllModelsCacheMu.Unlock()

	if entry == nil || entry.resp == nil {
		t.Fatalf("provider-less key must cache an empty catalog, got %#v", entry)
	}
	if len(entry.resp.Data) != 0 {
		t.Fatalf("expected an empty model list, got %d", len(entry.resp.Data))
	}
	if entry.done != nil {
		t.Fatalf("refresh must clear the in-flight marker")
	}
}

func TestRefreshListModels_UnsetProviderListStillFansOut(t *testing.T) {
	// The filter leaves the provider list unset when no live virtual key
	// scopes the request. That must not be mistaken for "granted nothing", or
	// an unscoped deployment would report an empty catalog.
	key := "no-vk:*:false"
	listAllModelsCacheMu.Lock()
	delete(listAllModelsCache, key)
	listAllModelsCacheMu.Unlock()
	t.Cleanup(func() {
		listAllModelsCacheMu.Lock()
		delete(listAllModelsCache, key)
		listAllModelsCacheMu.Unlock()
	})

	h := &CompletionHandler{} // nil client: returns before any fan-out
	done := make(chan struct{})
	h.refreshListModels(key, "vk-1", nil, false, "", false, done)
	<-done

	listAllModelsCacheMu.Lock()
	entry := listAllModelsCache[key]
	listAllModelsCacheMu.Unlock()
	if entry != nil && entry.resp != nil {
		t.Fatalf("unscoped request must not be cached as an empty catalog")
	}
}

// TestInvalidateListModelsCache_DropsAllEntries ensures a provider/key reload
// can force the next GET /v1/models to re-fan-out instead of serving a still-
// fresh 5-minute TTL entry that was populated before the upstream catalog
// changed. Without this, PUT /api/providers/{name} re-discovers live models
// but the public list endpoint keeps reporting the pre-reload snapshot.
func TestInvalidateListModelsCache_DropsAllEntries(t *testing.T) {
	listAllModelsCacheMu.Lock()
	listAllModelsCache = map[string]*listAllModelsCacheEntry{
		"vk-1:omp-gw:false": {
			resp: &schemas.BifrostListModelsResponse{
				Data: []schemas.Model{{ID: "omp-gw/old-model"}},
			},
			at: time.Now(),
		},
	}
	listAllModelsCacheMu.Unlock()
	t.Cleanup(func() {
		listAllModelsCacheMu.Lock()
		listAllModelsCache = map[string]*listAllModelsCacheEntry{}
		listAllModelsCacheMu.Unlock()
	})

	InvalidateListModelsCache()

	listAllModelsCacheMu.Lock()
	defer listAllModelsCacheMu.Unlock()
	if len(listAllModelsCache) != 0 {
		t.Fatalf("expected empty listAllModelsCache after InvalidateListModelsCache, got %#v", listAllModelsCache)
	}
}
