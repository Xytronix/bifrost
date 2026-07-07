package oauth2

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	bifrost "github.com/maximhq/bifrost/core"
	"github.com/maximhq/bifrost/core/schemas"
)

// TestFetchSingleAuthServerMetadataOIDCPathAppend pins the fix for issue #4371
// (gap 1): when the authorization-server issuer URL carries a path
// (e.g. https://replit.com/oidc), discovery must try the OIDC path-append form
// <issuer>/.well-known/openid-configuration. The RFC 8414 path-insertion form
// (/.well-known/oauth-authorization-server/<path>) is served here as 403 to
// mirror Replit, so success proves the path-append candidate is attempted.
func TestFetchSingleAuthServerMetadataOIDCPathAppend(t *testing.T) {
	SetLogger(bifrost.NewDefaultLogger(schemas.LogLevelError))

	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oidc/.well-known/openid-configuration": // OIDC path-append (the only working form)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(fmt.Sprintf(
				`{"issuer":%q,"authorization_endpoint":%q,"token_endpoint":%q,"code_challenge_methods_supported":["S256"]}`,
				srv.URL+"/oidc", srv.URL+"/oidc/auth", srv.URL+"/oidc/token")))
		case "/.well-known/oauth-authorization-server/oidc": // RFC 8414 path-insertion
			w.WriteHeader(http.StatusForbidden)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	metadata, err := fetchSingleAuthServerMetadata(context.Background(), srv.URL+"/oidc")
	require.NoError(t, err)
	require.NotNil(t, metadata)
	assert.Equal(t, srv.URL+"/oidc/auth", metadata.AuthorizationURL)
	assert.Equal(t, srv.URL+"/oidc/token", metadata.TokenURL)
}

// TestBuildAuthorizeURLWithPKCEIncludesResource pins issue #4371 (gap 2) on the
// authorization request: the RFC 8707 resource parameter is present when a
// resource is supplied and absent when it is empty.
func TestBuildAuthorizeURLWithPKCEIncludesResource(t *testing.T) {
	p := &OAuth2Provider{}
	const resource = "https://replit-mcp.com/server/mcp"

	raw := p.buildAuthorizeURLWithPKCE("https://replit.com/oidc/auth", "cid", "https://cb", "state123", "challenge", []string{"apps:read"}, resource)
	u, err := url.Parse(raw)
	require.NoError(t, err)
	assert.Equal(t, resource, u.Query().Get("resource"))

	rawEmpty := p.buildAuthorizeURLWithPKCE("https://replit.com/oidc/auth", "cid", "https://cb", "state123", "challenge", []string{"apps:read"}, "")
	uEmpty, err := url.Parse(rawEmpty)
	require.NoError(t, err)
	_, has := uEmpty.Query()["resource"]
	assert.False(t, has, "resource param must be omitted when empty")
}

// TestExchangeCodeForTokensWithPKCESendsResource pins issue #4371 (gap 2) on the
// token request: the RFC 8707 resource parameter is included in the auth-code
// exchange body.
func TestExchangeCodeForTokensWithPKCESendsResource(t *testing.T) {
	const resource = "https://replit-mcp.com/server/mcp"
	var gotResource, gotGrant string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		gotResource = r.PostForm.Get("resource")
		gotGrant = r.PostForm.Get("grant_type")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"abc","token_type":"bearer","expires_in":3600}`))
	}))
	defer srv.Close()

	p := NewOAuth2Provider(newTestConfigStore(), bifrost.NewDefaultLogger(schemas.LogLevelError))
	p.retryBaseDelay = time.Millisecond

	resp, err := p.exchangeCodeForTokensWithPKCE(context.Background(), srv.URL, "code", "cid", "secret", "https://cb", "verifier", resource)
	require.NoError(t, err)
	require.NotNil(t, resp)
	assert.Equal(t, "authorization_code", gotGrant)
	assert.Equal(t, resource, gotResource)
	assert.Equal(t, "abc", resp.AccessToken)
}

// TestExchangeRefreshTokenSendsResource pins issue #4371 (gap 2) on refresh: the
// refreshed token stays audience-scoped to the same MCP server via the resource
// parameter.
func TestExchangeRefreshTokenSendsResource(t *testing.T) {
	const resource = "https://replit-mcp.com/server/mcp"
	var gotResource, gotGrant string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		gotResource = r.PostForm.Get("resource")
		gotGrant = r.PostForm.Get("grant_type")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"new","token_type":"bearer","expires_in":3600}`))
	}))
	defer srv.Close()

	p := NewOAuth2Provider(newTestConfigStore(), bifrost.NewDefaultLogger(schemas.LogLevelError))
	p.retryBaseDelay = time.Millisecond

	resp, err := p.exchangeRefreshToken(context.Background(), srv.URL, "cid", "secret", "refreshtok", resource)
	require.NoError(t, err)
	require.NotNil(t, resp)
	assert.Equal(t, "refresh_token", gotGrant)
	assert.Equal(t, resource, gotResource)
}
