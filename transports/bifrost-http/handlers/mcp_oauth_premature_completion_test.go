package handlers

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/configstore/tables"
	"github.com/maximhq/bifrost/framework/oauth2"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
)

// TestIsPrematureOAuthCompletion pins the signal that catches a
// shared-OAuth reauthorize completing without its callback ever actually
// running (e.g. the upstream/mock authorization server rejected the request
// before redirecting back — a dead-end error page, not an OAuth redirect).
// TableOauthConfig.Status can't be used for this: it's write-once and never
// regresses once "authorized" (see its own doc comment), so a client
// reauthorizing after an earlier successful auth always reads stale
// "authorized" throughout a failed attempt. CompleteOAuthFlow always cleans
// up the flow row on completion, success or failure alike — so a row still
// sitting there pending (and not yet expired) means the callback never ran
// at all, and completion must be rejected rather than reconnecting with
// whatever (possibly still-broken) credential was already on file.
func TestIsPrematureOAuthCompletion(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	future := now.Add(time.Hour)
	past := now.Add(-time.Hour)

	tests := []struct {
		name string
		flow *tables.TableMCPOauthFlow
		want bool
	}{
		{
			name: "nil flow (already cleaned up by a genuine completion) is not premature",
			flow: nil,
			want: false,
		},
		{
			name: "pending, unexpired flow means the callback never ran: premature",
			flow: &tables.TableMCPOauthFlow{Status: "pending", ExpiresAt: future},
			want: true,
		},
		{
			name: "pending but expired flow (abandoned attempt) does not block a later fresh completion",
			flow: &tables.TableMCPOauthFlow{Status: "pending", ExpiresAt: past},
			want: false,
		},
		{
			name: "failed flow row is not premature (already resolved, just unsuccessfully)",
			flow: &tables.TableMCPOauthFlow{Status: "failed", ExpiresAt: future},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isPrematureOAuthCompletion(tt.flow, now)
			if got != tt.want {
				t.Errorf("isPrematureOAuthCompletion(%+v, %v) = %v, want %v", tt.flow, now, got, tt.want)
			}
		})
	}
}

func TestGetOAuthConfigStatusReportsPendingReauthorizationFlow(t *testing.T) {
	const (
		oauthConfigID = "oauth-config"
		mcpClientID   = "mcp-client"
	)
	now := time.Now()
	expiresAt := now.Add(time.Hour)
	store := &mockOAuth2Store{
		oauthConfigs: map[string]*tables.TableOauthConfig{
			oauthConfigID: {
				ID:     oauthConfigID,
				Status: "authorized",
			},
		},
		mcpClientsByOauthConfig: map[string]*tables.TableMCPClient{
			oauthConfigID: {
				ClientID: mcpClientID,
				AuthType: string(schemas.MCPAuthTypeOauth),
			},
		},
		adminFlowsByClient: map[string]*tables.TableMCPOauthFlow{
			mcpClientID: {
				MCPClientID: mcpClientID,
				FlowMode:    string(schemas.MCPAuthModeAdmin),
				Status:      "pending",
				UpdatedAt:   now.Add(-time.Hour),
				ExpiresAt:   expiresAt,
			},
		},
		sharedTokensByConfig: map[string]*tables.TableMCPOauthToken{
			oauthConfigID: {
				OauthConfigID: oauthConfigID,
				AuthMode:      "shared",
				Status:        "needs_reauth",
				UpdatedAt:     now,
			},
		},
	}
	handler := NewOAuthHandler(nil, nil, newTestOAuth2Config(store, tables.MCPServerAuthModeBoth, false))
	ctx := &fasthttp.RequestCtx{}
	ctx.SetUserValue("id", oauthConfigID)

	handler.getOAuthConfigStatus(ctx)

	require.Equal(t, fasthttp.StatusOK, ctx.Response.StatusCode())
	var response map[string]any
	require.NoError(t, json.Unmarshal(ctx.Response.Body(), &response))
	require.Equal(t, "pending", response["status"])
}

func TestGetOAuthConfigStatusIgnoresFlowOlderThanCredential(t *testing.T) {
	const (
		oauthConfigID = "oauth-config"
		mcpClientID   = "mcp-client"
	)
	now := time.Now()
	store := &mockOAuth2Store{
		oauthConfigs: map[string]*tables.TableOauthConfig{
			oauthConfigID: {
				ID:     oauthConfigID,
				Status: "authorized",
			},
		},
		mcpClientsByOauthConfig: map[string]*tables.TableMCPClient{
			oauthConfigID: {
				ClientID: mcpClientID,
				AuthType: string(schemas.MCPAuthTypeOauth),
			},
		},
		adminFlowsByClient: map[string]*tables.TableMCPOauthFlow{
			mcpClientID: {
				MCPClientID: mcpClientID,
				FlowMode:    string(schemas.MCPAuthModeAdmin),
				Status:      "failed",
				UpdatedAt:   now.Add(-time.Hour),
				ExpiresAt:   now.Add(time.Hour),
			},
		},
		sharedTokensByConfig: map[string]*tables.TableMCPOauthToken{
			oauthConfigID: {
				OauthConfigID: oauthConfigID,
				AuthMode:      "shared",
				Status:        "active",
				UpdatedAt:     now,
			},
		},
	}
	handler := NewOAuthHandler(nil, nil, newTestOAuth2Config(store, tables.MCPServerAuthModeBoth, false))
	ctx := &fasthttp.RequestCtx{}
	ctx.SetUserValue("id", oauthConfigID)

	handler.getOAuthConfigStatus(ctx)

	require.Equal(t, fasthttp.StatusOK, ctx.Response.StatusCode())
	var response map[string]any
	require.NoError(t, json.Unmarshal(ctx.Response.Body(), &response))
	require.Equal(t, "authorized", response["status"])
}

func TestGetOAuthConfigStatusUsesPerUserStagingCredentialToIgnoreOldFailure(t *testing.T) {
	const (
		oauthConfigID = "oauth-config"
		mcpClientID   = "mcp-client"
	)
	now := time.Now()
	store := &mockOAuth2Store{
		oauthConfigs: map[string]*tables.TableOauthConfig{
			oauthConfigID: {
				ID:     oauthConfigID,
				Status: "authorized",
			},
		},
		mcpClientsByOauthConfig: map[string]*tables.TableMCPClient{
			oauthConfigID: {
				ClientID: mcpClientID,
				AuthType: string(schemas.MCPAuthTypePerUserOauth),
			},
		},
		adminFlowsByClient: map[string]*tables.TableMCPOauthFlow{
			mcpClientID: {
				MCPClientID: mcpClientID,
				FlowMode:    string(schemas.MCPAuthModeAdmin),
				Status:      "failed",
				UpdatedAt:   now.Add(-time.Hour),
				ExpiresAt:   now.Add(time.Hour),
			},
		},
		adminTokensByClient: map[string]*tables.TableMCPOauthToken{
			mcpClientID: {
				MCPClientID: mcpClientID,
				AuthMode:    "admin",
				Status:      "needs_reauth",
				UpdatedAt:   now.Add(-2 * time.Hour),
			},
		},
		sharedTokensByConfig: map[string]*tables.TableMCPOauthToken{
			oauthConfigID: {
				MCPClientID:   mcpClientID,
				OauthConfigID: oauthConfigID,
				AuthMode:      "shared",
				Status:        "active",
				UpdatedAt:     now,
			},
		},
	}
	handler := NewOAuthHandler(nil, nil, newTestOAuth2Config(store, tables.MCPServerAuthModeBoth, false))
	ctx := &fasthttp.RequestCtx{}
	ctx.SetUserValue("id", oauthConfigID)

	handler.getOAuthConfigStatus(ctx)

	require.Equal(t, fasthttp.StatusOK, ctx.Response.StatusCode())
	var response map[string]any
	require.NoError(t, json.Unmarshal(ctx.Response.Body(), &response))
	require.Equal(t, "authorized", response["status"])
}

func TestCompleteMCPClientOAuthKeepsPendingReauthorizationOpen(t *testing.T) {
	const (
		oauthConfigID = "oauth-config"
		mcpClientID   = "mcp-client"
	)
	SetLogger(&mockLogger{})
	store := &mockOAuth2Store{
		oauthConfigs: map[string]*tables.TableOauthConfig{
			oauthConfigID: {
				ID:     oauthConfigID,
				Status: "authorized",
			},
		},
		mcpClientsByOauthConfig: map[string]*tables.TableMCPClient{
			oauthConfigID: {
				ClientID: mcpClientID,
				AuthType: string(schemas.MCPAuthTypeOauth),
			},
		},
		adminFlowsByClient: map[string]*tables.TableMCPOauthFlow{
			mcpClientID: {
				MCPClientID: mcpClientID,
				FlowMode:    string(schemas.MCPAuthModeAdmin),
				Status:      "pending",
				ExpiresAt:   time.Now().Add(time.Hour),
			},
		},
	}
	config := newTestOAuth2Config(store, tables.MCPServerAuthModeBoth, false)
	oauthProvider := oauth2.NewOAuth2Provider(store, &mockLogger{})
	oauthHandler := NewOAuthHandler(oauthProvider, nil, config)
	handler := NewMCPHandler(nil, nil, nil, config, oauthHandler, nil)
	ctx := &fasthttp.RequestCtx{}
	ctx.SetUserValue("id", oauthConfigID)

	handler.completeMCPClientOAuth(ctx)

	require.Equal(t, http.StatusTooEarly, ctx.Response.StatusCode())
}
