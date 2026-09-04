package handlers

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/configstore"
	configstoreTables "github.com/maximhq/bifrost/framework/configstore/tables"
	"github.com/maximhq/bifrost/transports/bifrost-http/lib"
	"github.com/valyala/fasthttp"
)

type mockListModelsVKConfigStore struct {
	configstore.ConfigStore
	vk  *configstoreTables.TableVirtualKey
	err error
}

func (m *mockListModelsVKConfigStore) GetVirtualKeyByValue(_ context.Context, _ string) (*configstoreTables.TableVirtualKey, error) {
	return m.vk, m.err
}

func TestApplyListModelsVirtualKeyProviderFilterSetsActiveVKProviders(t *testing.T) {
	h := &CompletionHandler{
		config: &lib.Config{
			ConfigStore: &mockListModelsVKConfigStore{vk: &configstoreTables.TableVirtualKey{
				Value:    *schemas.NewSecretVar("sk-bf-active"),
				IsActive: new(true),
				ProviderConfigs: []configstoreTables.TableVirtualKeyProviderConfig{
					{Provider: "openai"},
					{Provider: " anthropic "},
					{Provider: ""},
				},
			}},
		},
	}

	ctx := &fasthttp.RequestCtx{}
	ctx.Request.Header.Set("Authorization", "Bearer sk-bf-active")
	bifrostCtx := schemas.NewBifrostContext(context.Background(), time.Time{})

	if ok := h.applyListModelsVirtualKeyProviderFilter(ctx, bifrostCtx); !ok {
		t.Fatalf("expected active VK to apply provider filter")
	}
	got, ok := bifrostCtx.Value(schemas.BifrostContextKeyAvailableProviders).([]schemas.ModelProvider)
	if !ok {
		t.Fatalf("expected available providers to be set")
	}
	want := []schemas.ModelProvider{schemas.OpenAI, schemas.Anthropic}
	if len(got) != len(want) {
		t.Fatalf("expected providers %#v, got %#v", want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("expected providers %#v, got %#v", want, got)
		}
	}
}

func TestApplyListModelsVirtualKeyProviderFilterReturnsErrorOnLookupFailure(t *testing.T) {
	h := &CompletionHandler{
		config: &lib.Config{
			ConfigStore: &mockListModelsVKConfigStore{err: errors.New("database unavailable")},
		},
	}

	ctx := &fasthttp.RequestCtx{}
	ctx.Request.Header.Set("Authorization", "Bearer sk-bf-active")
	bifrostCtx := schemas.NewBifrostContext(context.Background(), time.Time{})

	if ok := h.applyListModelsVirtualKeyProviderFilter(ctx, bifrostCtx); ok {
		t.Fatalf("expected lookup error to fail request")
	}
	if got := ctx.Response.StatusCode(); got != fasthttp.StatusInternalServerError {
		t.Fatalf("expected status %d, got %d", fasthttp.StatusInternalServerError, got)
	}
	if body := string(ctx.Response.Body()); !strings.Contains(body, "Failed to resolve virtual key") {
		t.Fatalf("expected virtual key lookup error response, got %q", body)
	}
}

func TestApplyListModelsVirtualKeyProviderFilterReturnsUnavailableWithoutConfigStore(t *testing.T) {
	h := &CompletionHandler{config: &lib.Config{}}

	ctx := &fasthttp.RequestCtx{}
	ctx.Request.Header.Set("Authorization", "Bearer sk-bf-active")
	bifrostCtx := schemas.NewBifrostContext(context.Background(), time.Time{})

	if ok := h.applyListModelsVirtualKeyProviderFilter(ctx, bifrostCtx); ok {
		t.Fatalf("expected missing config store to fail request")
	}
	if got := ctx.Response.StatusCode(); got != fasthttp.StatusServiceUnavailable {
		t.Fatalf("expected status %d, got %d", fasthttp.StatusServiceUnavailable, got)
	}
	if body := string(ctx.Response.Body()); !strings.Contains(body, "database store unavailable") {
		t.Fatalf("expected unavailable response, got %q", body)
	}
}

func TestApplyListModelsVirtualKeyProviderFilterSkipsWhenVKNotFound(t *testing.T) {
	h := &CompletionHandler{
		config: &lib.Config{
			ConfigStore: &mockListModelsVKConfigStore{},
		},
	}

	ctx := &fasthttp.RequestCtx{}
	ctx.Request.Header.Set("Authorization", "Bearer sk-bf-missing")
	bifrostCtx := schemas.NewBifrostContext(context.Background(), time.Time{})

	if ok := h.applyListModelsVirtualKeyProviderFilter(ctx, bifrostCtx); !ok {
		t.Fatalf("expected missing VK to be ignored without failing request")
	}
	if got := bifrostCtx.Value(schemas.BifrostContextKeyAvailableProviders); got != nil {
		t.Fatalf("expected missing VK not to set available providers, got %#v", got)
	}
}

func TestApplyListModelsVirtualKeyProviderFilterSkipsInactiveVK(t *testing.T) {
	h := &CompletionHandler{
		config: &lib.Config{
			ConfigStore: &mockListModelsVKConfigStore{vk: &configstoreTables.TableVirtualKey{
				Value:    *schemas.NewSecretVar("sk-bf-inactive"),
				IsActive: new(false),
				ProviderConfigs: []configstoreTables.TableVirtualKeyProviderConfig{
					{Provider: "openai"},
				},
			}},
		},
	}

	ctx := &fasthttp.RequestCtx{}
	ctx.Request.Header.Set("Authorization", "Bearer sk-bf-inactive")
	bifrostCtx := schemas.NewBifrostContext(context.Background(), time.Time{})

	if ok := h.applyListModelsVirtualKeyProviderFilter(ctx, bifrostCtx); !ok {
		t.Fatalf("expected inactive VK to be ignored without failing request")
	}
	if got := bifrostCtx.Value(schemas.BifrostContextKeyAvailableProviders); got != nil {
		t.Fatalf("expected inactive VK not to set available providers, got %#v", got)
	}
}

func TestPreparePublicListModelsRequestDoesNotExposeUnfiltered(t *testing.T) {
	ctx := &fasthttp.RequestCtx{}
	ctx.QueryArgs().Set("page_size", "25")
	ctx.QueryArgs().Set("page_token", "next")
	ctx.QueryArgs().Set("unfiltered", "true")
	ctx.QueryArgs().Set("byProvider", "anthropic")

	req := preparePublicListModelsRequest(ctx, "bedrock")

	if req.Unfiltered {
		t.Fatal("public GET /v1/models must not enable the internal unfiltered flag")
	}
	if req.PageSize != 25 || req.PageToken != "next" || req.Provider != "bedrock" {
		t.Fatalf("request fields were not preserved: %#v", req)
	}
	if got := req.ExtraParams["byProvider"]; got != "anthropic" {
		t.Fatalf("provider query filter got %#v, want anthropic", got)
	}
	if _, leaked := req.ExtraParams["unfiltered"]; leaked {
		t.Fatal("unfiltered must be ignored rather than forwarded upstream")
	}
}

func TestListModelsCacheableRejectsRequestSpecificInputs(t *testing.T) {
	plainCtx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	if !listModelsCacheable(plainCtx, &schemas.BifrostListModelsRequest{Provider: "openai"}) {
		t.Fatal("plain list-models request should use the cache")
	}

	withParams := &schemas.BifrostListModelsRequest{
		Provider:    "bedrock",
		ExtraParams: map[string]interface{}{"byProvider": "anthropic"},
	}
	if listModelsCacheable(plainCtx, withParams) {
		t.Fatal("provider-specific query filters must bypass the shared cache")
	}

	for _, tc := range []struct {
		name  string
		key   schemas.BifrostContextKey
		value interface{}
	}{
		{name: "key name", key: schemas.BifrostContextKeyAPIKeyName, value: "preferred"},
		{name: "key id", key: schemas.BifrostContextKeyAPIKeyID, value: "key-id"},
		{name: "direct key", key: schemas.BifrostContextKeyDirectKey, value: schemas.Key{ID: "direct"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
			ctx.SetValue(tc.key, tc.value)
			if listModelsCacheable(ctx, &schemas.BifrostListModelsRequest{Provider: "openai"}) {
				t.Fatal("explicit key selection must bypass the shared cache")
			}
		})
	}
}
func TestListModelsCached_KeyFormattingAndHit(t *testing.T) {
	h := &CompletionHandler{
		config: &lib.Config{},
	}

	// Clean/reset the cache for test isolation
	listAllModelsCacheMu.Lock()
	listAllModelsCache = map[string]*listAllModelsCacheEntry{}

	// Pre-populate cache entries
	entry1 := &listAllModelsCacheEntry{
		resp: &schemas.BifrostListModelsResponse{
			Data: []schemas.Model{
				{ID: "openai/gpt-4o"},
			},
		},
		at: time.Now(),
	}
	listAllModelsCache["vk-1:openai:true"] = entry1

	entry2 := &listAllModelsCacheEntry{
		resp: &schemas.BifrostListModelsResponse{
			Data: []schemas.Model{
				{ID: "anthropic/claude-3"},
			},
		},
		at: time.Now(),
	}
	listAllModelsCache["vk-2:*:false"] = entry2
	listAllModelsCacheMu.Unlock()

	// Case 1: vk-1, openai, unfiltered=true -> should hit entry1
	ctx1 := schemas.NewBifrostContext(context.Background(), time.Time{})
	ctx1.SetValue(schemas.BifrostContextKeyVirtualKey, "vk-1")
	req1 := &schemas.BifrostListModelsRequest{
		Provider:   "openai",
		Unfiltered: true,
	}

	resp1, err1 := h.listModelsCached(ctx1, req1)
	if err1 != nil {
		t.Fatalf("unexpected error: %v", err1)
	}
	if len(resp1.Data) != 1 || resp1.Data[0].ID != "openai/gpt-4o" {
		t.Fatalf("expected openai/gpt-4o, got %#v", resp1.Data)
	}

	// Case 2: vk-2, no provider, unfiltered=false -> should hit entry2
	ctx2 := schemas.NewBifrostContext(context.Background(), time.Time{})
	ctx2.SetValue(schemas.BifrostContextKeyVirtualKey, "vk-2")
	req2 := &schemas.BifrostListModelsRequest{
		Provider:   "",
		Unfiltered: false,
	}

	resp2, err2 := h.listModelsCached(ctx2, req2)
	if err2 != nil {
		t.Fatalf("unexpected error: %v", err2)
	}
	if len(resp2.Data) != 1 || resp2.Data[0].ID != "anthropic/claude-3" {
		t.Fatalf("expected anthropic/claude-3, got %#v", resp2.Data)
	}

	// Case 3: vk-1, openai, unfiltered=false -> should NOT hit entry1 (cache miss, since unfiltered differs)
	// Since client is nil, a cache miss should attempt background refresh and block on cold start,
	// eventually failing/hanging or timing out. We can test this by checking that it returns error or times out,
	// or by cancelling the context.
	ctx3, cancel3 := context.WithCancel(context.Background())
	bCtx3 := schemas.NewBifrostContext(ctx3, time.Time{})
	bCtx3.SetValue(schemas.BifrostContextKeyVirtualKey, "vk-1")
	cancel3() // cancel immediately to fail the cold start wait

	req3 := &schemas.BifrostListModelsRequest{
		Provider:   "openai",
		Unfiltered: false,
	}

	_, err3 := h.listModelsCached(bCtx3, req3)
	if err3 == nil {
		t.Fatalf("expected error on cancelled context cache miss, got nil")
	}
}
