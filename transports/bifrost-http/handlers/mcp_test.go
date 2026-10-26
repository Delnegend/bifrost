package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"

	bifrost "github.com/maximhq/bifrost/core"
	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/configstore"
	configtables "github.com/maximhq/bifrost/framework/configstore/tables"
	"github.com/maximhq/bifrost/framework/mcp_headers"
	"github.com/maximhq/bifrost/framework/oauth2"
	"github.com/maximhq/bifrost/transports/v2/bifrost-http/lib"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
)

// TestMain points the OAuth discovery/registration client at an unguarded
// dialer for this package: the MCP OAuth tests here register against
// loopback-bound httptest.Server instances, which the production guard in
// framework/oauth2 refuses to dial. The guard has its own coverage there.
//
// It also installs the package logger before any test runs. Production sets
// it at startup, and code such as the WebSocket panic recovery logs through it
// unconditionally, so a test that reaches that code must not depend on an
// earlier test in the run having set it.
func TestMain(m *testing.M) {
	oauth2.SetDiscoveryDialContextForTests((&net.Dialer{}).DialContext)
	SetLogger(&mockLogger{})
	os.Exit(m.Run())
}

// =============================================================================
// POST /api/mcp/client/{id}/reregister and /reauthorize, driven through
// startMCPClientReauthorization against a real sqlite store, a real
// OAuth2Provider, and a fake RFC 7591 authorization server. Only the two
// collaborators whose calls are under test (the credential cache and the MCP
// manager) are fakes.
// =============================================================================

// reregisterAuthServer is the registration half of an authorization server,
// with the two answers the handler has to tell apart scripted in.
type reregisterAuthServer struct {
	mu            sync.Mutex
	registrations int
	refuseWith    int    // when non-zero, /register answers with this status
	reissue       string // when set, /register hands back this client_id
	server        *httptest.Server
}

func newReregisterAuthServer(t *testing.T) *reregisterAuthServer {
	t.Helper()
	as := &reregisterAuthServer{}
	mux := http.NewServeMux()
	mux.HandleFunc("/register", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		as.mu.Lock()
		if as.refuseWith != 0 {
			status := as.refuseWith
			as.mu.Unlock()
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": "invalid_client_metadata"})
			return
		}
		as.registrations++
		clientID := as.reissue
		if clientID == "" {
			clientID = fmt.Sprintf("dyn-client-%d", as.registrations)
		}
		as.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"client_id": clientID})
	})
	as.server = httptest.NewServer(mux)
	t.Cleanup(as.server.Close)
	return as
}

func (as *reregisterAuthServer) set(refuseWith int, reissue string) {
	as.mu.Lock()
	defer as.mu.Unlock()
	as.refuseWith, as.reissue = refuseWith, reissue
}

func (as *reregisterAuthServer) registrationCount() int {
	as.mu.Lock()
	defer as.mu.Unlock()
	return as.registrations
}

// recordingCredentialCache embeds the interface so any eviction other than the
// one under test panics rather than passing unnoticed.
type recordingCredentialCache struct {
	MCPCredentialCacheManager
	evictedClients          []string
	evictedDiscoveryClients []string
}

func (c *recordingCredentialCache) EvictOauthTokenCacheByMCPClient(_ context.Context, mcpClientID string) {
	c.evictedClients = append(c.evictedClients, mcpClientID)
}

func (c *recordingCredentialCache) EvictMCPPerUserDiscoveryCacheByMCPClient(_ context.Context, mcpClientID string) {
	c.evictedDiscoveryClients = append(c.evictedDiscoveryClients, mcpClientID)
}

// recordingMCPManager embeds the interface for the same reason.
type recordingMCPManager struct {
	MCPManager
	closedClients []string
}

func (m *recordingMCPManager) CloseAndMarkNeedsReauth(_ context.Context, id string) error {
	m.closedClients = append(m.closedClients, id)
	return nil
}

// flowFailingStore is the real store with one failure scripted in: the flow row
// the consent step needs cannot be written. Everything before it, the rotation
// included, goes through to sqlite and commits for real.
type flowFailingStore struct {
	configstore.ConfigStore
	failFlowCreate bool
}

func (s *flowFailingStore) CreateOauthUserSession(ctx context.Context, session *configtables.TableMCPOauthFlow) error {
	if s.failFlowCreate {
		return errors.New("database is locked")
	}
	return s.ConfigStore.CreateOauthUserSession(ctx, session)
}

const reregisterMCPClientID = "mcp-grafana"

type reregisterFixture struct {
	handler            *MCPHandler
	store              *flowFailingStore
	as                 *reregisterAuthServer
	cache              *recordingCredentialCache
	manager            *recordingMCPManager
	oauthConfigID      string
	registeredClientID string
	lastCtx            *fasthttp.RequestCtx // the request the last call ran, for audit assertions
}

// newReregisterFixture bootstraps a shared-OAuth MCP client whose client_id was
// issued through dynamic registration, which is the only kind of client the
// reregister route has anything to do for.
func newReregisterFixture(t *testing.T) *reregisterFixture {
	t.Helper()
	SetLogger(&mockLogger{})
	ctx := context.Background()

	as := newReregisterAuthServer(t)
	store := &flowFailingStore{ConfigStore: newRealOAuth2Store(t)}
	provider := oauth2.NewOAuth2Provider(store, bifrost.NewDefaultLogger(schemas.LogLevelError))

	initiation, err := provider.InitiateOAuthFlow(ctx, &schemas.OAuth2Config{
		AuthorizeURL:    as.server.URL + "/authorize",
		TokenURL:        as.server.URL + "/token",
		RegistrationURL: bifrost.Ptr(as.server.URL + "/register"),
		RedirectURI:     "http://localhost:8080/api/oauth/callback",
		ServerURL:       as.server.URL,
		Scopes:          []string{"openid"},
	})
	require.NoError(t, err)
	require.Equal(t, 1, as.registrationCount(), "bootstrap must dynamically register exactly one client")

	bootstrapped, err := store.GetOauthConfigByID(ctx, initiation.OauthConfigID)
	require.NoError(t, err)
	require.NotNil(t, bootstrapped)

	require.NoError(t, store.CreateMCPClientConfig(ctx, &schemas.MCPClientConfig{
		ID:               reregisterMCPClientID,
		Name:             "grafana",
		ConnectionType:   schemas.MCPConnectionTypeHTTP,
		ConnectionString: schemas.NewSecretVar(as.server.URL + "/mcp"),
		AuthType:         schemas.MCPAuthTypeOauth,
		OauthConfigID:    bifrost.Ptr(initiation.OauthConfigID),
	}))

	cache := &recordingCredentialCache{}
	manager := &recordingMCPManager{}
	cfg := &lib.Config{
		ConfigStore:   store,
		ClientConfig:  &configstore.ClientConfig{},
		OAuthProvider: provider,
	}
	return &reregisterFixture{
		handler:            &MCPHandler{store: cfg, mcpManager: manager, mcpCredentialCacheManager: cache},
		store:              store,
		as:                 as,
		cache:              cache,
		manager:            manager,
		oauthConfigID:      initiation.OauthConfigID,
		registeredClientID: bootstrapped.GetResolvedClientID(),
	}
}

// call runs one of the two routes and returns the status and decoded body.
func (f *reregisterFixture) call(t *testing.T, route func(*fasthttp.RequestCtx)) (int, map[string]any, string) {
	t.Helper()
	var req fasthttp.Request
	req.Header.SetMethod(fasthttp.MethodPost)
	req.Header.SetHost("bifrost.example.com")
	ctx := initCtx(&req)
	ctx.SetUserValue("id", reregisterMCPClientID)

	route(ctx)
	f.lastCtx = ctx

	raw := string(ctx.Response.Body())
	var body map[string]any
	_ = json.Unmarshal(ctx.Response.Body(), &body)
	return ctx.Response.StatusCode(), body, raw
}

func (f *reregisterFixture) storedClientID(t *testing.T) string {
	t.Helper()
	row, err := f.store.GetOauthConfigByID(context.Background(), f.oauthConfigID)
	require.NoError(t, err)
	require.NotNil(t, row)
	return row.GetResolvedClientID()
}

// TestReregisterMCPClient_InvalidatesInMemoryStateWhenItRotates pins that a
// rotation through this route leaves nothing in memory still serving the
// credentials it just invalidated.
//
// RotateMCPOAuthConfig cascades every bound token to needs_reauth in the
// database, but two things outlive that: the OAuth provider's token cache, which
// GetUserAccessTokenByMode answers from before it ever reads a row, and a
// shared client's live connection, which has the old bearer baked into its
// transport. The update-client route has always dropped both after rotating;
// this route rotates through the same store call and has to as well. Eviction
// goes through the handler's cache manager rather than the provider directly
// because a clustered deployment overrides it to notify peers.
func TestReregisterMCPClient_InvalidatesInMemoryStateWhenItRotates(t *testing.T) {
	f := newReregisterFixture(t)

	status, body, raw := f.call(t, f.handler.reregisterMCPClient)
	require.Equal(t, fasthttp.StatusOK, status, raw)
	require.NotEqual(t, f.registeredClientID, body["registered_client_id"], "fixture check: a replacement was issued")

	assert.Equal(t, []string{reregisterMCPClientID}, f.cache.evictedClients,
		"cached tokens issued under the replaced client must be evicted, or they keep being served over a needs_reauth row")
	assert.Equal(t, []string{reregisterMCPClientID}, f.manager.closedClients,
		"the live connection still carries the replaced client's bearer and must be closed")
}

// TestReregisterMCPClient_DropsCallersDiscoveriesWhenItRotates: for a client
// that keeps a discovery per caller, the rotation's needs_reauth cascade deletes
// every caller's discovery too, and every node drops its cached copies.
func TestReregisterMCPClient_DropsCallersDiscoveriesWhenItRotates(t *testing.T) {
	f := newReregisterFixture(t)
	ctx := context.Background()
	row, err := f.store.GetMCPClientByID(ctx, reregisterMCPClientID)
	require.NoError(t, err)
	row.PerUserDiscovery = schemas.Ptr(true)
	require.NoError(t, f.store.UpdateMCPClientConfig(ctx, reregisterMCPClientID, row))
	seedDiscovery(t, f.store, schemas.MCPAuthModeUser, "alice", reregisterMCPClientID)

	status, _, raw := f.call(t, f.handler.reregisterMCPClient)
	require.Equal(t, fasthttp.StatusOK, status, raw)

	assert.Nil(t, storedDiscovery(f.store, schemas.MCPAuthModeUser, "alice", reregisterMCPClientID))
	assert.Equal(t, []string{reregisterMCPClientID}, f.cache.evictedDiscoveryClients)
}

// TestReregisterMCPClient_LeavesInMemoryStateAloneWhenNothingRotated is the
// other side of the gate: a provider that hands back the registration it
// already holds invalidated nothing, so there is nothing to tear down, and
// closing a working connection over it would be self-inflicted.
func TestReregisterMCPClient_LeavesInMemoryStateAloneWhenNothingRotated(t *testing.T) {
	f := newReregisterFixture(t)
	f.as.set(0, f.registeredClientID)

	status, body, raw := f.call(t, f.handler.reregisterMCPClient)
	require.Equal(t, fasthttp.StatusOK, status, raw)
	require.Equal(t, f.registeredClientID, body["registered_client_id"], "fixture check: the provider re-issued the same client")

	assert.Empty(t, f.cache.evictedClients)
	assert.Empty(t, f.manager.closedClients)
}

// TestReauthorizeMCPClient_NeverInvalidatesInMemoryState keeps the new
// invalidation on its own side of the route split: plain reauthorize rotates
// nothing and must not start behaving as though it had.
func TestReauthorizeMCPClient_NeverInvalidatesInMemoryState(t *testing.T) {
	f := newReregisterFixture(t)

	status, _, raw := f.call(t, f.handler.reauthorizeMCPClient)
	require.Equal(t, fasthttp.StatusOK, status, raw)

	assert.Equal(t, 1, f.as.registrationCount(), "plain reauthorize must not register anything")
	assert.Empty(t, f.cache.evictedClients)
	assert.Empty(t, f.manager.closedClients)
}

// TestReregisterMCPClient_ReportsACommittedRotationWhenConsentSetupFails pins
// what the caller is told when the route half-succeeds.
//
// Registration and rotation commit before the consent flow is opened, and the
// upstream registration cannot be rolled back, so a failure opening the flow
// leaves the replacement client installed. A bare "failed to initiate
// reauthorization" reads as "nothing happened, try again", and trying again
// registers yet another client. The response has to say the rotation is done,
// name both ids, and point at the route that finishes the job without
// registering anything.
func TestReregisterMCPClient_ReportsACommittedRotationWhenConsentSetupFails(t *testing.T) {
	f := newReregisterFixture(t)
	f.store.failFlowCreate = true

	status, _, raw := f.call(t, f.handler.reregisterMCPClient)

	replacement := f.storedClientID(t)
	require.NotEqual(t, f.registeredClientID, replacement, "fixture check: the rotation committed before the failure")

	assert.Equal(t, fasthttp.StatusInternalServerError, status)
	assert.Contains(t, raw, replacement, "must name the client_id that is now installed")
	assert.Contains(t, raw, f.registeredClientID, "must name the client_id it replaced")
	assert.Contains(t, raw, "reauthorize", "must point at the route that completes consent without registering another client")
	assert.Contains(t, raw, "database is locked", "must still carry the underlying cause")

	assert.Equal(t, []string{reregisterMCPClientID}, f.cache.evictedClients,
		"the rotation committed, so its invalidation must have run regardless of what failed after it")
}

func TestReregisterMCPClient_AuditsCommittedRotationWhenConsentSetupFails(t *testing.T) {
	f := newReregisterFixture(t)
	f.store.failFlowCreate = true

	f.call(t, f.handler.reregisterMCPClient)

	change := requireSingleAuditChange(t, f.lastCtx, "mcp_client.reregistered")
	assert.Equal(t, reregisterMCPClientID, change.TargetID)
}

// TestReauthorizeMCPClient_ConsentSetupFailure_ClaimsNoRotation is the
// counterpart: with nothing committed there is nothing to report, and the
// plain failure message stays what it was.
func TestReauthorizeMCPClient_ConsentSetupFailure_ClaimsNoRotation(t *testing.T) {
	f := newReregisterFixture(t)
	f.store.failFlowCreate = true

	status, _, raw := f.call(t, f.handler.reauthorizeMCPClient)

	assert.Equal(t, fasthttp.StatusInternalServerError, status)
	assert.Contains(t, raw, "Failed to initiate reauthorization")
	assert.NotContains(t, raw, "registered")
}

// TestReregisterMCPClient_StatusSaysWhoseProblemItIs pins the status each
// registration failure is reported under. 400 tells the caller its request can
// be changed to succeed, which is true of exactly two failures: there is no
// registration endpoint to ask (set one, or reauthorize instead), and the
// provider understood the request and refused it. A provider outage is neither,
// and reporting it as the caller's mistake sends them to fix a request that was
// never wrong.
func TestReregisterMCPClient_StatusSaysWhoseProblemItIs(t *testing.T) {
	t.Run("no registration endpoint is the caller's to fix", func(t *testing.T) {
		f := newReregisterFixture(t)
		ctx := context.Background()
		row, err := f.store.GetOauthConfigByID(ctx, f.oauthConfigID)
		require.NoError(t, err)
		row.RegistrationURL = nil
		require.NoError(t, f.store.UpdateOauthConfig(ctx, row))

		status, _, raw := f.call(t, f.handler.reregisterMCPClient)
		assert.Equal(t, fasthttp.StatusBadRequest, status, raw)
		assert.Contains(t, raw, "registration endpoint")
	})

	t.Run("a provider refusal is the caller's to fix", func(t *testing.T) {
		f := newReregisterFixture(t)
		f.as.set(http.StatusForbidden, "")

		status, _, raw := f.call(t, f.handler.reregisterMCPClient)
		assert.Equal(t, fasthttp.StatusBadRequest, status, raw)
		assert.Contains(t, raw, "status 403")
	})

	t.Run("a provider outage is not", func(t *testing.T) {
		f := newReregisterFixture(t)
		f.as.set(http.StatusServiceUnavailable, "")

		status, _, raw := f.call(t, f.handler.reregisterMCPClient)
		assert.Equal(t, fasthttp.StatusInternalServerError, status, raw)
		assert.Contains(t, raw, "status 503")
		assert.Equal(t, f.registeredClientID, f.storedClientID(t), "a failed attempt must leave the stored client alone")
		assert.Empty(t, f.cache.evictedClients, "nothing rotated, so nothing may be invalidated")
	})
}

// =============================================================================
// POST /api/mcp/client with auth_type oauth against an OAuth-protected MCP
// server on loopback, with the production OAuth dialer.
// =============================================================================

// newLoopbackOAuthMCPServer serves the RFC 8414 discovery document and RFC 7591
// registration an OAuth-protected MCP server on the operator's own host
// exposes (the shape of examples/mcps/oauth-demo-server).
func newLoopbackOAuthMCPServer(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	var server *httptest.Server
	mux.HandleFunc("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                 server.URL,
			"authorization_endpoint": server.URL + "/authorize",
			"token_endpoint":         server.URL + "/token",
			"registration_endpoint":  server.URL + "/register",
		})
	})
	mux.HandleFunc("/register", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"client_id": "loopback-client"})
	})
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})
	server = httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}

// TestAddMCPClient_OAuthOnLoopbackFollowsPrivateAllowlist pins the fix for MCP
// OAuth servers on localhost: once the OAuth chain was put behind the
// public-only dialer, an authenticated admin could not add one at all. With
// the server's host on mcp_oauth_private_allowlist the admin's registration
// discovers, registers and returns an authorize URL; without it the chain is
// still refused, and a caller with auth bypassed is refused regardless.
func TestAddMCPClient_OAuthOnLoopbackFollowsPrivateAllowlist(t *testing.T) {
	oauth2.SetDiscoveryDialContextForTests(nil)
	t.Cleanup(func() {
		oauth2.SetDiscoveryDialContextForTests((&net.Dialer{}).DialContext)
		require.NoError(t, lib.ApplyMCPOAuthPrivateAllowlist(nil))
	})

	for _, tc := range []struct {
		name         string
		allow        []string
		authBypassed bool
		wantStatus   int
		wantBody     string
	}{
		{name: "authenticated, loopback allowlisted", allow: []string{"127.0.0.1"}, wantStatus: fasthttp.StatusOK, wantBody: "authorize_url"},
		{name: "authenticated, no allowlist", wantStatus: fasthttp.StatusInternalServerError, wantBody: "blocked connection to non-public address"},
		{name: "auth bypassed, loopback allowlisted", allow: []string{"127.0.0.1"}, authBypassed: true, wantStatus: fasthttp.StatusForbidden, wantBody: "unauthenticated callers cannot register MCP clients"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			SetLogger(&mockLogger{})
			require.NoError(t, lib.ApplyMCPOAuthPrivateAllowlist(tc.allow))
			server := newLoopbackOAuthMCPServer(t)
			store := newRealOAuth2Store(t)
			provider := oauth2.NewOAuth2Provider(store, bifrost.NewDefaultLogger(schemas.LogLevelError))
			cfg := &lib.Config{ConfigStore: store, ClientConfig: &configstore.ClientConfig{}, OAuthProvider: provider}
			h := &MCPHandler{store: cfg, mcpManager: failingMCPManager{}, oauthHandler: NewOAuthHandler(provider, nil, cfg, nil)}

			var req fasthttp.Request
			req.Header.SetMethod(fasthttp.MethodPost)
			req.Header.SetContentType("application/json")
			req.Header.SetHost("bifrost.example.com")
			req.SetBodyString(`{"name":"local_oauth","connection_type":"http","connection_string":"` + server.URL + `/mcp","auth_type":"oauth","oauth_config":{}}`)
			ctx := initCtx(&req)
			if tc.authBypassed {
				ctx.SetUserValue(schemas.BifrostContextKeyAuthBypassed, true)
			}
			h.addMCPClient(ctx)

			body := string(ctx.Response.Body())
			require.Equal(t, tc.wantStatus, ctx.Response.StatusCode(), body)
			assert.Contains(t, body, tc.wantBody)
		})
	}
}

// removeRecordingMCPManager records RemoveMCPClient calls; any other manager call panics.
type removeRecordingMCPManager struct {
	MCPManager
	removed []string
}

func (m *removeRecordingMCPManager) RemoveMCPClient(_ context.Context, id string) error {
	m.removed = append(m.removed, id)
	return nil
}

// TestDeleteMCPClient_UnknownIDIsNotFound pins that deleting an MCP client id with
// no row answers 404 and leaves the runtime manager untouched, while a known id is
// still removed from both the store and the manager.
func TestDeleteMCPClient_UnknownIDIsNotFound(t *testing.T) {
	SetLogger(&mockLogger{})
	store := newRealOAuth2Store(t)
	manager := &removeRecordingMCPManager{}
	h := &MCPHandler{store: &lib.Config{ConfigStore: store, ClientConfig: &configstore.ClientConfig{}}, mcpManager: manager}
	del := func(id string) (int, string) {
		var req fasthttp.Request
		req.Header.SetMethod(fasthttp.MethodDelete)
		ctx := initCtx(&req)
		ctx.SetUserValue("id", id)
		h.deleteMCPClient(ctx)
		return ctx.Response.StatusCode(), string(ctx.Response.Body())
	}

	status, body := del("3f8e2a1c-0000-4000-8000-000000000000")
	assert.Equal(t, fasthttp.StatusNotFound, status, body)
	assert.Contains(t, body, "not found")
	assert.Empty(t, manager.removed, "an unknown id must not reach the runtime manager")

	require.NoError(t, store.CreateMCPClientConfig(context.Background(), &schemas.MCPClientConfig{
		ID:               "mcp-delete-known",
		Name:             "delete_known",
		ConnectionType:   schemas.MCPConnectionTypeHTTP,
		ConnectionString: schemas.NewSecretVar("https://example.invalid/mcp"),
	}))
	status, body = del("mcp-delete-known")
	assert.Equal(t, fasthttp.StatusOK, status, body)
	assert.Equal(t, []string{"mcp-delete-known"}, manager.removed)
	_, err := store.GetMCPClientByID(context.Background(), "mcp-delete-known")
	assert.ErrorIs(t, err, configstore.ErrNotFound)
}

// =============================================================================
// GET /api/mcp/library installed flag
// =============================================================================

// libraryInstalledStore serves one library page and a client table of any size,
// honoring the limit/offset the handler asks for.
type libraryInstalledStore struct {
	configstore.ConfigStore
	entries     []configtables.TableMCPLibrary
	clients     []configtables.TableMCPClient
	clientPages int
}

func (s *libraryInstalledStore) GetMCPLibraryPaginated(_ context.Context, _ configstore.MCPLibraryQueryParams) ([]configtables.TableMCPLibrary, int64, error) {
	return s.entries, int64(len(s.entries)), nil
}

func (s *libraryInstalledStore) GetMCPClientsPaginated(_ context.Context, params configstore.MCPClientsQueryParams) ([]configtables.TableMCPClient, int64, error) {
	s.clientPages++
	if params.Offset >= len(s.clients) {
		return nil, int64(len(s.clients)), nil
	}
	end := min(params.Offset+params.Limit, len(s.clients))
	return s.clients[params.Offset:end], int64(len(s.clients)), nil
}

func fillerMCPClients(n int) []configtables.TableMCPClient {
	clients := make([]configtables.TableMCPClient, n)
	for i := range clients {
		clients[i] = configtables.TableMCPClient{
			ClientID:         fmt.Sprintf("filler-%d", i),
			Name:             fmt.Sprintf("filler_%d", i),
			EndpointSlug:     fmt.Sprintf("filler-%d", i),
			ConnectionType:   string(schemas.MCPConnectionTypeHTTP),
			ConnectionString: schemas.NewSecretVar(fmt.Sprintf("https://filler.example.com/%d", i)),
		}
	}
	return clients
}

func TestGetMCPLibrary_InstalledLooksPastFirstHundredClients(t *testing.T) {
	clients := append(fillerMCPClients(250), configtables.TableMCPClient{
		ClientID:         "context7",
		Name:             "Context7",
		EndpointSlug:     "context7",
		ConnectionType:   string(schemas.MCPConnectionTypeHTTP),
		ConnectionString: schemas.NewSecretVar("https://mcp.context7.com/mcp"),
	})
	store := &libraryInstalledStore{
		entries: []configtables.TableMCPLibrary{
			{Slug: "context7", Name: "Context7", ConnectionType: schemas.MCPConnectionTypeHTTP, ConnectionURL: "https://mcp.context7.com/mcp"},
			{Slug: "linear", Name: "Linear", ConnectionType: schemas.MCPConnectionTypeHTTP, ConnectionURL: "https://mcp.linear.app/mcp"},
		},
		clients: clients,
	}
	h := &MCPHandler{store: &lib.Config{ConfigStore: store}}

	ctx := &fasthttp.RequestCtx{}
	h.getMCPLibrary(ctx)

	require.Equal(t, fasthttp.StatusOK, ctx.Response.StatusCode(), string(ctx.Response.Body()))
	var resp struct {
		Servers []struct {
			Slug      string `json:"slug"`
			Installed bool   `json:"installed"`
		} `json:"servers"`
	}
	require.NoError(t, json.Unmarshal(ctx.Response.Body(), &resp))
	require.Len(t, resp.Servers, 2)
	assert.Equal(t, "context7", resp.Servers[0].Slug)
	assert.True(t, resp.Servers[0].Installed, "client #251 installs context7")
	assert.Equal(t, "linear", resp.Servers[1].Slug)
	assert.False(t, resp.Servers[1].Installed)
	assert.Equal(t, 3, store.clientPages, "251 clients read in pages of 100")
}

func TestIsMCPLibraryEntryInstalled(t *testing.T) {
	httpEntry := configtables.TableMCPLibrary{
		Name:           "Context7",
		ConnectionType: schemas.MCPConnectionTypeHTTP,
		ConnectionURL:  "https://mcp.context7.com/mcp",
	}
	stdioEntry := configtables.TableMCPLibrary{
		Name:           "File System",
		ConnectionType: schemas.MCPConnectionTypeSTDIO,
		StdioConfig:    &schemas.MCPStdioConfig{Command: "npx", Args: []string{"-y", "@modelcontextprotocol/server-filesystem"}},
	}
	cases := []struct {
		name   string
		entry  configtables.TableMCPLibrary
		client configtables.TableMCPClient
		want   bool
	}{
		{
			name:   "http renamed client still matches by url",
			entry:  httpEntry,
			client: configtables.TableMCPClient{Name: "docs_lookup", EndpointSlug: "context7", ConnectionString: schemas.NewSecretVar("https://mcp.context7.com/mcp")},
			want:   true,
		},
		{
			name:   "http url with trailing slash matches",
			entry:  httpEntry,
			client: configtables.TableMCPClient{Name: "docs_lookup", EndpointSlug: "docs-lookup", ConnectionString: schemas.NewSecretVar("https://mcp.context7.com/mcp/")},
			want:   true,
		},
		{
			name:   "stdio renamed client matches by immutable endpoint slug",
			entry:  stdioEntry,
			client: configtables.TableMCPClient{Name: "my_files", EndpointSlug: "file-system", StdioConfig: &schemas.MCPStdioConfig{Command: "npx", Args: []string{"-y", "other"}}},
			want:   true,
		},
		{
			name:   "stdio renamed client matches by command and args",
			entry:  stdioEntry,
			client: configtables.TableMCPClient{Name: "my_files", EndpointSlug: "my-files", StdioConfig: &schemas.MCPStdioConfig{Command: "npx", Args: []string{"-y", "@modelcontextprotocol/server-filesystem"}}},
			want:   true,
		},
		{
			name:   "stdio client with same command but other args does not match",
			entry:  stdioEntry,
			client: configtables.TableMCPClient{Name: "my_files", EndpointSlug: "my-files", StdioConfig: &schemas.MCPStdioConfig{Command: "npx", Args: []string{"-y", "other"}}},
			want:   false,
		},
		{
			name:   "name match is case-insensitive",
			entry:  stdioEntry,
			client: configtables.TableMCPClient{Name: "file_system", EndpointSlug: "custom"},
			want:   true,
		},
		{
			name:   "unrelated client does not match",
			entry:  httpEntry,
			client: configtables.TableMCPClient{Name: "other", EndpointSlug: "other", ConnectionString: schemas.NewSecretVar("https://other.example.com/mcp")},
			want:   false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := isMCPLibraryEntryInstalled(&tc.entry, []configtables.TableMCPClient{tc.client})
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestSanitizeMCPLibraryServerName(t *testing.T) {
	cases := map[string]string{
		"Context7":         "Context7",
		"Maxim AI":         "Maxim_AI",
		"  File - System ": "File_System",
		"a-_b":             "a__b",
		"-lead":            "lead",
		"1Password":        "mcp_1Password",
		"***":              "mcp_",
	}
	for in, want := range cases {
		assert.Equal(t, want, sanitizeMCPLibraryServerName(in), in)
	}
}

// TestUpdateMCPClient_DropsCallersCachedDiscoveriesOfAPerUserDiscoveryClient:
// an update of a client that keeps a discovery per caller may delete their
// discoveries (here, by adding a required header key), so every node drops its
// cached copies. Any other client's update evicts nothing.
func TestUpdateMCPClient_DropsCallersCachedDiscoveriesOfAPerUserDiscoveryClient(t *testing.T) {
	for _, perUser := range []bool{true, false} {
		t.Run(fmt.Sprintf("per_user_discovery=%v", perUser), func(t *testing.T) {
			h, _ := newOpenAPITestHandler(t)
			log := &evictionLog{}
			h.mcpCredentialCacheManager = log
			client := headersNotesClient()
			createSignInClient(t, h.store.ConfigStore, client, perUser)
			h.store.MCPConfig.ClientConfigs = []*schemas.MCPClientConfig{client}
			if perUser {
				seedDiscovery(t, h.store.ConfigStore, schemas.MCPAuthModeUser, "alice", client.ID)
			}

			status, body := doMCPRequest(t, h.updateMCPClient, fasthttp.MethodPut, `{"per_user_header_keys":["x-api-key","x-team"]}`, client.ID, false)
			require.Equal(t, fasthttp.StatusOK, status, body)

			if perUser {
				assert.Nil(t, storedDiscovery(h.store.ConfigStore, schemas.MCPAuthModeUser, "alice", client.ID), "callers resubmit their headers, then rediscover")
				assert.Contains(t, log.logged(), "discoveries of "+client.ID)
			} else {
				assert.NotContains(t, log.logged(), "discoveries of "+client.ID)
			}
		})
	}
}

// TestUpdateMCPClient_TurningPerUserDiscoveryOnOrOff: an update turns
// per_user_discovery on or off. The store applies what the change deletes,
// every node drops its cached copies, and the runtime is handed the record of
// the admin step the store reset. The flag is refused where it cannot apply.
func TestUpdateMCPClient_TurningPerUserDiscoveryOnOrOff(t *testing.T) {
	ctx := context.Background()
	setup := func(t *testing.T, client *schemas.MCPClientConfig, perUser bool) (*MCPHandler, *openAPIRecordingManager, *evictionLog) {
		h, mgr := newOpenAPITestHandler(t)
		log := &evictionLog{}
		h.mcpCredentialCacheManager = log
		createSignInClient(t, h.store.ConfigStore, client, perUser)
		h.store.MCPConfig.ClientConfigs = []*schemas.MCPClientConfig{client}
		return h, mgr, log
	}

	t.Run("on", func(t *testing.T) {
		client := headersNotesClient()
		h, mgr, log := setup(t, client, false)
		tools := map[string]schemas.ChatTool{"notes-search": {Type: schemas.ChatToolTypeFunction, Function: &schemas.ChatToolFunction{Name: "notes-search"}}}
		require.NoError(t, h.store.ConfigStore.UpdateMCPClientTools(ctx, client.ID, tools, map[string]string{"search": "search"}, ""))
		require.NoError(t, h.store.ConfigStore.UpsertMCPPerUserHeaderCredential(ctx, &configtables.TableMCPPerUserHeaderCredential{MCPClientID: client.ID, AuthMode: "admin", Status: "active", HeadersJSON: `{"x-api-key":"sample"}`}))

		status, body := doMCPRequest(t, h.updateMCPClient, fasthttp.MethodPut, `{"per_user_discovery":true}`, client.ID, false)
		require.Equal(t, fasthttp.StatusOK, status, body)

		stored, err := h.store.ConfigStore.GetMCPClientConfigByID(ctx, client.ID)
		require.NoError(t, err)
		assert.Equal(t, schemas.Ptr(true), stored.PerUserDiscovery)
		assert.Nil(t, stored.DiscoveredTools, "the admin's tools are not kept")
		admin, err := h.store.ConfigStore.GetAdminMCPPerUserHeaderCredentialsByClientIDs(ctx, []string{client.ID})
		require.NoError(t, err)
		assert.Empty(t, admin, "nor is the admin's credential")
		require.Len(t, mgr.updated, 1)
		assert.Equal(t, schemas.Ptr(true), mgr.updated[0].PerUserDiscovery)
		assert.False(t, mgr.updated[0].PendingVerification, "still verified")
		assert.Contains(t, log.logged(), "tokens of "+client.ID)
		assert.Contains(t, log.logged(), "header credentials of "+client.ID)
	})

	t.Run("off", func(t *testing.T) {
		client := headersNotesClient()
		h, mgr, log := setup(t, client, true)
		seedDiscovery(t, h.store.ConfigStore, schemas.MCPAuthModeUser, "alice", client.ID)

		status, body := doMCPRequest(t, h.updateMCPClient, fasthttp.MethodPut, `{"per_user_discovery":false}`, client.ID, false)
		require.Equal(t, fasthttp.StatusOK, status, body)

		assert.Nil(t, storedDiscovery(h.store.ConfigStore, schemas.MCPAuthModeUser, "alice", client.ID))
		require.Len(t, mgr.updated, 1)
		assert.Equal(t, schemas.Ptr(false), mgr.updated[0].PerUserDiscovery)
		assert.True(t, mgr.updated[0].PendingVerification, "back to its admin step: it stored no tools while the flag was on")
		assert.Contains(t, log.logged(), "discoveries of "+client.ID)
	})

	t.Run("refused for a shared auth type", func(t *testing.T) {
		client := headersNotesClient()
		client.AuthType = schemas.MCPAuthTypeHeaders
		client.PerUserHeaderKeys = nil
		h, mgr, _ := setup(t, client, false)

		status, body := doMCPRequest(t, h.updateMCPClient, fasthttp.MethodPut, `{"per_user_discovery":true}`, client.ID, false)
		assert.Equal(t, fasthttp.StatusBadRequest, status, body)
		assert.Contains(t, body, "per_user_discovery is only valid for per-user auth types")
		stored, err := h.store.ConfigStore.GetMCPClientConfigByID(ctx, client.ID)
		require.NoError(t, err)
		assert.Equal(t, schemas.Ptr(false), stored.PerUserDiscovery)
		assert.Empty(t, mgr.updated)
	})

	// A flip that committed has deleted what the client kept, which no rollback
	// brings back: a failure after it keeps the flip, and the same request sent
	// again finds the store already flipped and brings the runtime up to it.
	t.Run("a failure after the flip commits keeps it", func(t *testing.T) {
		for _, tc := range []struct {
			name   string
			breaks func(h *MCPHandler, mgr *openAPIRecordingManager) (repair func())
		}{
			{name: "reading it back", breaks: func(h *MCPHandler, _ *openAPIRecordingManager) func() {
				store := &readBackFailingStore{ConfigStore: h.store.ConfigStore}
				h.store.ConfigStore = store
				return func() { h.store.ConfigStore = store.ConfigStore }
			}},
			{name: "the runtime update", breaks: func(h *MCPHandler, mgr *openAPIRecordingManager) func() {
				mgr.updateErr = errors.New("reconnect already in progress for this client")
				return func() { mgr.updateErr = nil }
			}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				client := headersNotesClient()
				h, mgr, _ := setup(t, client, false)
				tools := map[string]schemas.ChatTool{"notes-search": {Type: schemas.ChatToolTypeFunction, Function: &schemas.ChatToolFunction{Name: "notes-search"}}}
				require.NoError(t, h.store.ConfigStore.UpdateMCPClientTools(ctx, client.ID, tools, map[string]string{"search": "search"}, ""))
				require.NoError(t, h.store.ConfigStore.UpsertMCPPerUserHeaderCredential(ctx, &configtables.TableMCPPerUserHeaderCredential{MCPClientID: client.ID, AuthMode: "admin", Status: "active", HeadersJSON: `{"x-api-key":"sample"}`}))
				repair := tc.breaks(h, mgr)

				status, body := doMCPRequest(t, h.updateMCPClient, fasthttp.MethodPut, `{"per_user_discovery":true}`, client.ID, false)
				require.Equal(t, fasthttp.StatusInternalServerError, status, body)
				repair()
				stored, err := h.store.ConfigStore.GetMCPClientConfigByID(ctx, client.ID)
				require.NoError(t, err)
				assert.Equal(t, schemas.Ptr(true), stored.PerUserDiscovery, "the flip that deleted the admin's credential stands")

				status, body = doMCPRequest(t, h.updateMCPClient, fasthttp.MethodPut, `{"per_user_discovery":true}`, client.ID, false)
				require.Equal(t, fasthttp.StatusOK, status, body)
				require.NotEmpty(t, mgr.updated)
				last := mgr.updated[len(mgr.updated)-1]
				assert.Equal(t, schemas.Ptr(true), last.PerUserDiscovery, "sending it again brings the runtime up to the store")
				assert.False(t, last.PendingVerification, "still verified: no second flip ran over the first")
			})
		}
	})
}

// readBackFailingStore fails every read of an MCP client's config once the
// client has been written, as a database that drops out right after a commit.
type readBackFailingStore struct {
	configstore.ConfigStore
	written bool
}

func (s *readBackFailingStore) UpdateMCPClientConfig(ctx context.Context, id string, client *configtables.TableMCPClient) error {
	if err := s.ConfigStore.UpdateMCPClientConfig(ctx, id, client); err != nil {
		return err
	}
	s.written = true
	return nil
}

func (s *readBackFailingStore) GetMCPClientConfigByID(ctx context.Context, id string) (*schemas.MCPClientConfig, error) {
	if s.written {
		return nil, errors.New("connection reset by peer")
	}
	return s.ConfigStore.GetMCPClientConfigByID(ctx, id)
}

// TestGetMCPClients_APerUserDiscoveryClientListsTheToolNamesItsCallersSee: a
// client with per_user_discovery on has no tools of its own, so the list gives
// the names seen in its callers' discoveries with how many callers see each,
// none until someone connects, and no credential of its own to show or to
// project a state from.
func TestGetMCPClients_APerUserDiscoveryClientListsTheToolNamesItsCallersSee(t *testing.T) {
	ctx := context.Background()
	notes := headersNotesClient()
	notes.PerUserDiscovery = schemas.Ptr(true)
	wiki := headersNotesClient()
	wiki.ID, wiki.Name, wiki.PerUserDiscovery = "client-wiki", "wiki", schemas.Ptr(true)
	h, _ := newVerificationHandler(t, newVerificationRuntime(t, notes, wiki))
	for _, client := range []*schemas.MCPClientConfig{notes, wiki} {
		require.NoError(t, h.store.ConfigStore.CreateMCPClientConfig(ctx, client))
	}
	tool := func(names ...string) map[string]schemas.ChatTool {
		tools := make(map[string]schemas.ChatTool, len(names))
		for _, name := range names {
			tools[name] = schemas.ChatTool{Type: schemas.ChatToolTypeFunction, Function: &schemas.ChatToolFunction{Name: name, Description: schemas.Ptr("seen by one caller")}}
		}
		return tools
	}
	for _, discovery := range []*configtables.TableMCPPerUserDiscovery{
		{MCPClientID: notes.ID, AuthMode: "user", UserID: schemas.Ptr("alice"), Tools: tool("search", "read"), DiscoveryHash: "hash-shared"},
		{MCPClientID: notes.ID, AuthMode: "user", UserID: schemas.Ptr("bob"), Tools: tool("search", "read"), DiscoveryHash: "hash-shared"},
		{MCPClientID: notes.ID, AuthMode: "vk", VirtualKeyID: schemas.Ptr("vk-1"), Tools: tool("search"), DiscoveryHash: "hash-vk-1"},
	} {
		require.NoError(t, h.store.ConfigStore.UpsertMCPPerUserDiscovery(ctx, discovery))
	}
	// An admin credential left from before the flag was on is not the client's.
	require.NoError(t, h.store.ConfigStore.UpsertMCPPerUserHeaderCredential(ctx, &configtables.TableMCPPerUserHeaderCredential{MCPClientID: notes.ID, AuthMode: "admin", Status: "needs_update", HeadersJSON: `{"x-api-key":"old"}`}))

	status, body := doMCPRequest(t, h.getMCPClients, fasthttp.MethodGet, "", "", false)
	require.Equal(t, fasthttp.StatusOK, status, body)
	var list struct {
		Clients []MCPClientResponse `json:"clients"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &list), body)
	listed := map[string]MCPClientResponse{}
	for _, client := range list.Clients {
		listed[client.Config.ID] = client
	}

	got := listed[notes.ID]
	assert.Equal(t, []schemas.ChatToolFunction{{Name: "read"}, {Name: "search"}}, got.Tools, "names only, sorted")
	assert.Equal(t, map[string]int{"search": 3, "read": 2}, got.CallersByTool)
	assert.Nil(t, got.Credential)
	assert.Equal(t, schemas.MCPConnectionStateHealthy, got.State, "no credential state is projected")
	assert.NotContains(t, body, "seen by one caller", "no caller's description is shown")

	assert.Empty(t, listed[wiki.ID].Tools, "no tools until someone connects")
	assert.Nil(t, listed[wiki.ID].CallersByTool)
}

// verificationRecorder stands in for the runtime on the admin verification
// routes: verification finds one tool, and it records whether the result was
// handed over as tools or as a config recording the verification.
type verificationRecorder struct {
	MCPManager
	mu       sync.Mutex
	toolsSet []string
	verified []string
	added    []*schemas.MCPClientConfig
}

func (r *verificationRecorder) verifiedTools(config *schemas.MCPClientConfig) (map[string]schemas.ChatTool, map[string]string, string, error) {
	name := config.Name + "-search"
	return map[string]schemas.ChatTool{name: {Type: schemas.ChatToolTypeFunction, Function: &schemas.ChatToolFunction{Name: name}}}, map[string]string{"search": "search"}, "", nil
}

func (r *verificationRecorder) VerifyHeadersConnection(_ context.Context, config *schemas.MCPClientConfig, _ map[string]string) (map[string]schemas.ChatTool, map[string]string, string, error) {
	return r.verifiedTools(config)
}

func (r *verificationRecorder) VerifyPerUserOAuthConnection(_ context.Context, config *schemas.MCPClientConfig, _ string) (map[string]schemas.ChatTool, map[string]string, string, error) {
	return r.verifiedTools(config)
}

func (r *verificationRecorder) UpdateMCPClient(context.Context, string, *schemas.MCPClientConfig) error {
	return nil
}

func (r *verificationRecorder) AddMCPClient(_ context.Context, config *schemas.MCPClientConfig) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.added = append(r.added, config)
	return nil
}

func (r *verificationRecorder) RequiresPerCallConnection(*schemas.MCPClientConfig) bool { return true }

func (r *verificationRecorder) SetClientTools(clientID string, _ map[string]schemas.ChatTool, _ map[string]string, _ string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.toolsSet = append(r.toolsSet, clientID)
}

func (r *verificationRecorder) UpdateMCPClientCredentials(_ context.Context, id string, config *schemas.MCPClientConfig) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !config.PendingVerification {
		r.verified = append(r.verified, id)
	}
	return nil
}

// exchangeIdP is an identity-provider integration whose token endpoint is
// stubbed to issue "exchanged-admin".
type exchangeIdP struct{ tokenEndpoint string }

func (exchangeIdP) Available() bool { return true }

func (e exchangeIdP) Resolve(context.Context, *schemas.MCPClientConfig) (*schemas.TokenExchangeIdP, error) {
	return &schemas.TokenExchangeIdP{TokenEndpoint: e.tokenEndpoint, GrantShape: schemas.TokenExchangeGrantRFC8693}, nil
}

func (exchangeIdP) StoredSubjectToken(context.Context, string, bool) (string, error) {
	return "", errors.New("no stored sign-in")
}

// recordFailingStore is the real store with recording a verification failing.
type recordFailingStore struct {
	configstore.ConfigStore
}

func (recordFailingStore) ClearMCPClientPendingVerification(context.Context, string) error {
	return errors.New("database is locked")
}

// newVerificationHandler returns an MCP handler over a real store, with the
// providers the verification routes use and runtime as its engine.
func newVerificationHandler(t *testing.T, runtime *bifrost.Bifrost) (*MCPHandler, *verificationRecorder) {
	t.Helper()
	SetLogger(&mockLogger{})
	store := newRealOAuth2Store(t)
	logger := bifrost.NewDefaultLogger(schemas.LogLevelError)
	provider := oauth2.NewOAuth2Provider(store, logger)
	tokens := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "exchanged-admin", "issued_token_type": "urn:ietf:params:oauth:token-type:access_token", "token_type": "Bearer", "expires_in": 3600})
	}))
	t.Cleanup(tokens.Close)
	provider.SetTokenExchangeIdPResolver(exchangeIdP{tokenEndpoint: tokens.URL})
	cfg := &lib.Config{
		ConfigStore:        store,
		ClientConfig:       &configstore.ClientConfig{},
		MCPConfig:          &schemas.MCPConfig{},
		OAuthProvider:      provider,
		MCPHeadersProvider: mcp_headers.NewProvider(store, logger),
	}
	recorder := &verificationRecorder{}
	h := &MCPHandler{store: cfg, mcpManager: recorder, client: runtime, mcpCredentialCacheManager: &evictionLog{}}
	h.oauthHandler = NewOAuthHandler(provider, runtime, cfg, recorder)
	return h, recorder
}

// newVerificationRuntime returns an engine holding clients.
func newVerificationRuntime(t *testing.T, clients ...*schemas.MCPClientConfig) *bifrost.Bifrost {
	t.Helper()
	runtime, err := bifrost.Init(t.Context(), schemas.BifrostConfig{
		Account:   wsSpanTestAccount{},
		Logger:    bifrost.NewDefaultLogger(schemas.LogLevelError),
		MCPConfig: &schemas.MCPConfig{ClientConfigs: clients},
	})
	require.NoError(t, err)
	t.Cleanup(runtime.Shutdown)
	runtime.ConnectConfiguredMCPClients(t.Context())
	return runtime
}

// withInboundBearer has the request carry the admin's identity-provider token,
// as the auth layer sets it.
func withInboundBearer(t *testing.T, ctx *fasthttp.RequestCtx, token string) {
	t.Helper()
	bctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	bctx.SetValue(schemas.BifrostContextKeyMCPInboundBearer, token)
	ctx.SetUserValue(lib.FastHTTPUserValueBifrostContext, bctx)
	ctx.SetUserValue(lib.FastHTTPUserValueBifrostCancel, context.CancelFunc(func() {}))
}

// TestAdminVerification_RecordsTheStepAndKeepsOnlyWhatTheClientUses runs each
// admin verification route on a client still pending it. Every route records
// the step on the row. A client without per_user_discovery keeps the admin's
// credential and the tools found; a client with it keeps neither, and the
// runtime is handed the config recording the step rather than tools.
func TestAdminVerification_RecordsTheStepAndKeepsOnlyWhatTheClientUses(t *testing.T) {
	ctx := context.Background()
	for _, perUser := range []bool{false, true} {
		t.Run(fmt.Sprintf("per_user_headers/per_user_discovery=%v", perUser), func(t *testing.T) {
			client := &schemas.MCPClientConfig{
				ID: "client-notes", Name: "notes", ConnectionType: schemas.MCPConnectionTypeHTTP,
				ConnectionString: schemas.NewSecretVar("http://upstream.invalid/mcp"), AuthType: schemas.MCPAuthTypePerUserHeaders,
				PerUserHeaderKeys: []string{"x-api-key"}, ToolsToExecute: []string{"*"}, PendingVerification: true, PerUserDiscovery: schemas.Ptr(perUser),
			}
			h, recorder := newVerificationHandler(t, newVerificationRuntime(t, client))
			require.NoError(t, h.store.ConfigStore.CreateMCPClientConfig(ctx, client))

			status, body := doMCPRequest(t, h.verifyMCPClientHeaders, fasthttp.MethodPost, `{"user_headers":{"x-api-key":"sample"}}`, client.ID, false)
			require.Equal(t, fasthttp.StatusOK, status, body)

			stored, err := h.store.ConfigStore.GetMCPClientConfigByID(ctx, client.ID)
			require.NoError(t, err)
			assert.False(t, stored.PendingVerification, "the step is recorded")
			admin, err := h.store.ConfigStore.GetAdminMCPPerUserHeaderCredentialsByClientIDs(ctx, []string{client.ID})
			require.NoError(t, err)
			if perUser {
				assert.Nil(t, stored.DiscoveredTools)
				assert.Empty(t, admin, "the admin's sample headers are not kept")
				assert.Equal(t, []string{client.ID}, recorder.verified)
				assert.Empty(t, recorder.toolsSet)
			} else {
				assert.Contains(t, stored.DiscoveredTools, "notes-search")
				assert.Len(t, admin, 1)
				assert.Equal(t, []string{client.ID}, recorder.toolsSet)
				assert.Empty(t, recorder.verified)
			}
		})

		t.Run(fmt.Sprintf("token_exchange/per_user_discovery=%v", perUser), func(t *testing.T) {
			h, recorder := newVerificationHandler(t, newVerificationRuntime(t))
			client := &schemas.MCPClientConfig{
				ID: "client-jira", Name: "jira", ConnectionType: schemas.MCPConnectionTypeHTTP,
				ConnectionString: schemas.NewSecretVar("http://upstream.invalid/mcp"), AuthType: schemas.MCPAuthTypeTokenExchange,
				TokenExchange:  &schemas.MCPTokenExchangeConfig{Audience: "api://jira", ClientID: schemas.NewSecretVar("exchange-app")},
				ToolsToExecute: []string{"*"}, PendingVerification: true, PerUserDiscovery: schemas.Ptr(perUser),
			}
			require.NoError(t, h.store.ConfigStore.CreateMCPClientConfig(ctx, client))

			var req fasthttp.Request
			req.Header.SetMethod(fasthttp.MethodPost)
			rctx := initCtx(&req)
			rctx.SetUserValue("id", client.ID)
			withInboundBearer(t, rctx, "admin-idp-token")
			h.verifyMCPClientExchange(rctx)
			require.Equal(t, fasthttp.StatusOK, rctx.Response.StatusCode(), string(rctx.Response.Body()))

			stored, err := h.store.ConfigStore.GetMCPClientConfigByID(ctx, client.ID)
			require.NoError(t, err)
			assert.False(t, stored.PendingVerification, "the step is recorded")
			admin, err := h.store.ConfigStore.GetAdminOauthTokenByMCPClientID(ctx, client.ID)
			require.NoError(t, err)
			if perUser {
				assert.Nil(t, stored.DiscoveredTools)
				assert.Nil(t, admin, "the admin's exchanged token is not kept")
				assert.Equal(t, []string{client.ID}, recorder.verified)
				assert.Empty(t, recorder.toolsSet)
			} else {
				assert.Contains(t, stored.DiscoveredTools, "jira-search")
				assert.NotNil(t, admin)
				assert.Equal(t, []string{client.ID}, recorder.toolsSet)
				assert.Empty(t, recorder.verified)
			}
		})

		t.Run(fmt.Sprintf("per_user_oauth/per_user_discovery=%v", perUser), func(t *testing.T) {
			h, recorder := newVerificationHandler(t, newVerificationRuntime(t))
			require.NoError(t, h.store.ConfigStore.CreateOauthConfig(ctx, &configtables.TableOauthConfig{
				ID: "oauth-wiki", ClientID: schemas.NewSecretVar("bifrost"), TokenURL: "https://idp.invalid/token",
				AuthorizeURL: "https://idp.invalid/authorize", RedirectURI: "http://localhost/api/oauth/callback", Status: "authorized",
			}))
			client := &schemas.MCPClientConfig{
				ID: "client-wiki", Name: "wiki", ConnectionType: schemas.MCPConnectionTypeHTTP,
				ConnectionString: schemas.NewSecretVar("http://upstream.invalid/mcp"), AuthType: schemas.MCPAuthTypePerUserOauth,
				OauthConfigID: schemas.Ptr("oauth-wiki"), PendingOAuthConfig: &schemas.OAuth2Config{}, ToolsToExecute: []string{"*"},
				// Without the flag the stash alone marks it pending, as a client
				// created by a node from before the record would be.
				PendingVerification: perUser, PerUserDiscovery: schemas.Ptr(perUser),
			}
			require.NoError(t, h.store.ConfigStore.CreateMCPClientConfig(ctx, client))
			// The admin's sign-in, as the OAuth callback stores it.
			require.NoError(t, h.store.ConfigStore.CreateOauthToken(ctx, &configtables.TableMCPOauthToken{
				ID: "token-admin", AuthMode: "shared", OauthConfigID: "oauth-wiki", Status: "active", AccessToken: "admin-token", TokenType: "Bearer",
			}))

			status, body := doMCPRequest(t, h.completeMCPClientOAuth, fasthttp.MethodPost, "", "oauth-wiki", false)
			require.Equal(t, fasthttp.StatusOK, status, body)

			stored, err := h.store.ConfigStore.GetMCPClientConfigByID(ctx, client.ID)
			require.NoError(t, err)
			assert.False(t, stored.PendingVerification, "the step is recorded")
			assert.Nil(t, stored.PendingOAuthConfig)
			admin, err := h.store.ConfigStore.GetAdminOauthTokenByMCPClientID(ctx, client.ID)
			require.NoError(t, err)
			staging, err := h.store.ConfigStore.GetSharedOauthTokenByConfigID(ctx, "oauth-wiki")
			require.NoError(t, err)
			assert.Nil(t, staging, "the sign-in is promoted or deleted, never left behind")
			if perUser {
				assert.Nil(t, stored.DiscoveredTools)
				assert.Nil(t, admin, "the admin's sign-in is not kept")
				assert.Equal(t, []string{client.ID}, recorder.verified)
				assert.Empty(t, recorder.toolsSet)
			} else {
				assert.Contains(t, stored.DiscoveredTools, "wiki-search")
				assert.NotNil(t, admin)
				assert.Equal(t, []string{client.ID}, recorder.toolsSet)
				assert.Empty(t, recorder.verified)
			}
		})
	}
}

// TestReauthorizeMCPClient_RefusesAClientStillPendingVerification: a client
// still needing its first sign-in is sent to initiate-verification.
func TestReauthorizeMCPClient_RefusesAClientStillPendingVerification(t *testing.T) {
	f := newReregisterFixture(t)
	ctx := context.Background()
	row, err := f.store.GetMCPClientByID(ctx, reregisterMCPClientID)
	require.NoError(t, err)
	require.NoError(t, f.store.DB().Model(row).Updates(map[string]any{"pending_verification": true, "pending_oauth_config_json": `{}`}).Error)

	status, _, raw := f.call(t, f.handler.reauthorizeMCPClient)
	assert.Equal(t, fasthttp.StatusBadRequest, status, raw)
	assert.Contains(t, raw, "use initiate-verification instead")
}

// TestAddMCPClient_TokenExchangeWithoutTheAdminsTokenIsStoredPending: with no
// identity-provider token to verify as, the client is created pending its admin
// step, and that is what the row and the runtime are given.
func TestAddMCPClient_TokenExchangeWithoutTheAdminsTokenIsStoredPending(t *testing.T) {
	h, recorder := newVerificationHandler(t, newVerificationRuntime(t))
	ctx := context.Background()
	body := `{"name":"jira","connection_type":"http","connection_string":"http://upstream.invalid/mcp","auth_type":"token_exchange",` +
		`"token_exchange":{"audience":"api://jira","client_id":"exchange-app"},"tools_to_execute":["*"]}`
	// A deployment that supports token exchange, and no identity-provider token
	// on the request.
	bctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	bctx.SetValue(schemas.BifrostContextKeyIsEnterprise, true)
	var req fasthttp.Request
	req.Header.SetMethod(fasthttp.MethodPost)
	req.Header.SetContentType("application/json")
	req.SetBodyString(body)
	rctx := initCtx(&req)
	rctx.SetUserValue(lib.FastHTTPUserValueBifrostContext, bctx)
	rctx.SetUserValue(lib.FastHTTPUserValueBifrostCancel, context.CancelFunc(func() {}))
	h.addMCPClient(rctx)
	require.Equal(t, fasthttp.StatusOK, rctx.Response.StatusCode(), string(rctx.Response.Body()))

	require.Len(t, recorder.added, 1)
	assert.True(t, recorder.added[0].PendingVerification, "the runtime parks it")
	stored, err := h.store.ConfigStore.GetMCPClientConfigByID(ctx, recorder.added[0].ID)
	require.NoError(t, err)
	assert.True(t, stored.PendingVerification, "and the row records it")
}

// TestVerifyMCPClientHeaders_AFailedRecordFailsOnlyAClientReadFromIt: a client
// without per_user_discovery is read from the tools verification stores, so a
// failure to also record the step is logged and the verification succeeds. A
// client with it is read from the record alone, so the same failure fails the
// request, leaving the client pending and the retry open.
func TestVerifyMCPClientHeaders_AFailedRecordFailsOnlyAClientReadFromIt(t *testing.T) {
	ctx := context.Background()
	for _, perUser := range []bool{false, true} {
		t.Run(fmt.Sprintf("per_user_discovery=%v", perUser), func(t *testing.T) {
			client := &schemas.MCPClientConfig{
				ID: "client-notes", Name: "notes", ConnectionType: schemas.MCPConnectionTypeHTTP,
				ConnectionString: schemas.NewSecretVar("http://upstream.invalid/mcp"), AuthType: schemas.MCPAuthTypePerUserHeaders,
				PerUserHeaderKeys: []string{"x-api-key"}, ToolsToExecute: []string{"*"}, PendingVerification: true, PerUserDiscovery: schemas.Ptr(perUser),
			}
			h, recorder := newVerificationHandler(t, newVerificationRuntime(t, client))
			require.NoError(t, h.store.ConfigStore.CreateMCPClientConfig(ctx, client))
			h.store.ConfigStore = recordFailingStore{h.store.ConfigStore}

			status, body := doMCPRequest(t, h.verifyMCPClientHeaders, fasthttp.MethodPost, `{"user_headers":{"x-api-key":"sample"}}`, client.ID, false)

			stored, err := h.store.ConfigStore.GetMCPClientConfigByID(ctx, client.ID)
			require.NoError(t, err)
			if perUser {
				assert.Equal(t, fasthttp.StatusInternalServerError, status, body)
				assert.True(t, stored.PendingVerification, "still pending, so a retry re-runs verification")
				assert.Empty(t, recorder.verified)
			} else {
				assert.Equal(t, fasthttp.StatusOK, status, body)
				assert.False(t, stored.PendingVerification, "the stored tools record the step")
				assert.Equal(t, []string{client.ID}, recorder.toolsSet)
			}
		})
	}
}

// TestAdminCredentialRoutes_RefuseAVerifiedPerUserDiscoveryClient: a verified
// client with per_user_discovery on keeps no admin credential, so the routes that
// refresh one refuse it: reauthorize, and verify-headers or verify-exchange once
// the client is verified. Reregister still runs, since callers need the new
// client registration it makes.
func TestAdminCredentialRoutes_RefuseAVerifiedPerUserDiscoveryClient(t *testing.T) {
	ctx := context.Background()
	t.Run("reauthorize", func(t *testing.T) {
		f := newReregisterFixture(t)
		row, err := f.store.GetMCPClientByID(ctx, reregisterMCPClientID)
		require.NoError(t, err)
		require.NoError(t, f.store.DB().Model(row).Updates(map[string]any{"auth_type": string(schemas.MCPAuthTypePerUserOauth), "per_user_discovery": true}).Error)

		status, _, raw := f.call(t, f.handler.reauthorizeMCPClient)
		assert.Equal(t, fasthttp.StatusBadRequest, status, raw)
		assert.Contains(t, raw, "keeps no admin credential")

		status, _, raw = f.call(t, f.handler.reregisterMCPClient)
		assert.Equal(t, fasthttp.StatusOK, status, raw)
	})

	t.Run("verify-headers", func(t *testing.T) {
		h, recorder := newVerificationHandler(t, newVerificationRuntime(t))
		client := headersNotesClient()
		createSignInClient(t, h.store.ConfigStore, client, true)

		status, body := doMCPRequest(t, h.verifyMCPClientHeaders, fasthttp.MethodPost, `{"user_headers":{"x-api-key":"sample"}}`, client.ID, false)
		assert.Equal(t, fasthttp.StatusBadRequest, status, body)
		assert.Contains(t, body, "keeps no admin credential")
		assert.Empty(t, recorder.verified)
	})

	t.Run("verify-exchange", func(t *testing.T) {
		h, recorder := newVerificationHandler(t, newVerificationRuntime(t))
		client := &schemas.MCPClientConfig{
			ID: "client-jira", Name: "jira", ConnectionType: schemas.MCPConnectionTypeHTTP,
			ConnectionString: schemas.NewSecretVar("http://upstream.invalid/mcp"), AuthType: schemas.MCPAuthTypeTokenExchange,
			TokenExchange:  &schemas.MCPTokenExchangeConfig{Audience: "api://jira", ClientID: schemas.NewSecretVar("exchange-app")},
			ToolsToExecute: []string{"*"}, PerUserDiscovery: schemas.Ptr(true),
		}
		require.NoError(t, h.store.ConfigStore.CreateMCPClientConfig(ctx, client))

		var req fasthttp.Request
		req.Header.SetMethod(fasthttp.MethodPost)
		rctx := initCtx(&req)
		rctx.SetUserValue("id", client.ID)
		withInboundBearer(t, rctx, "admin-idp-token")
		h.verifyMCPClientExchange(rctx)
		assert.Equal(t, fasthttp.StatusBadRequest, rctx.Response.StatusCode(), string(rctx.Response.Body()))
		assert.Contains(t, string(rctx.Response.Body()), "keeps no admin credential")
		assert.Empty(t, recorder.verified)
	})
}

// TestCompleteMCPClientOAuth_ARepairSignInOfAPerUserDiscoveryClientKeepsNothing:
// an admin sign-in completing for a verified client with per_user_discovery on
// (after a new client registration, say) only verifies it: the sign-in is
// deleted and nothing found with it is kept.
func TestCompleteMCPClientOAuth_ARepairSignInOfAPerUserDiscoveryClientKeepsNothing(t *testing.T) {
	ctx := context.Background()
	h, recorder := newVerificationHandler(t, newVerificationRuntime(t))
	require.NoError(t, h.store.ConfigStore.CreateOauthConfig(ctx, &configtables.TableOauthConfig{
		ID: "oauth-wiki", ClientID: schemas.NewSecretVar("bifrost"), TokenURL: "https://idp.invalid/token",
		AuthorizeURL: "https://idp.invalid/authorize", RedirectURI: "http://localhost/api/oauth/callback", Status: "authorized",
	}))
	require.NoError(t, h.store.ConfigStore.CreateMCPClientConfig(ctx, &schemas.MCPClientConfig{
		ID: "client-wiki", Name: "wiki", ConnectionType: schemas.MCPConnectionTypeHTTP,
		ConnectionString: schemas.NewSecretVar("http://upstream.invalid/mcp"), AuthType: schemas.MCPAuthTypePerUserOauth,
		OauthConfigID: schemas.Ptr("oauth-wiki"), ToolsToExecute: []string{"*"}, PerUserDiscovery: schemas.Ptr(true),
	}))
	require.NoError(t, h.store.ConfigStore.CreateOauthToken(ctx, &configtables.TableMCPOauthToken{
		ID: "token-admin", AuthMode: "shared", OauthConfigID: "oauth-wiki", Status: "active", AccessToken: "admin-token", TokenType: "Bearer",
	}))

	status, body := doMCPRequest(t, h.completeMCPClientOAuth, fasthttp.MethodPost, "", "oauth-wiki", false)
	require.Equal(t, fasthttp.StatusOK, status, body)

	staging, err := h.store.ConfigStore.GetSharedOauthTokenByConfigID(ctx, "oauth-wiki")
	require.NoError(t, err)
	assert.Nil(t, staging, "the sign-in is deleted")
	admin, err := h.store.ConfigStore.GetAdminOauthTokenByMCPClientID(ctx, "client-wiki")
	require.NoError(t, err)
	assert.Nil(t, admin, "and not kept as the admin credential")
	stored, err := h.store.ConfigStore.GetMCPClientConfigByID(ctx, "client-wiki")
	require.NoError(t, err)
	assert.Nil(t, stored.DiscoveredTools)
	assert.Empty(t, recorder.toolsSet)
}

// TestAddMCPClient_WithPerUserDiscoveryKeepsNothingFromVerification: a client
// created with per_user_discovery on is verified the same way, and keeps neither
// the tools found nor the admin's credential. The flag is refused where it
// cannot apply.
func TestAddMCPClient_WithPerUserDiscoveryKeepsNothingFromVerification(t *testing.T) {
	ctx := context.Background()
	create := func(t *testing.T, h *MCPHandler, body string, values map[schemas.BifrostContextKey]any) (int, string) {
		t.Helper()
		bctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
		for key, value := range values {
			bctx.SetValue(key, value)
		}
		var req fasthttp.Request
		req.Header.SetMethod(fasthttp.MethodPost)
		req.Header.SetContentType("application/json")
		req.SetBodyString(body)
		rctx := initCtx(&req)
		rctx.SetUserValue(lib.FastHTTPUserValueBifrostContext, bctx)
		rctx.SetUserValue(lib.FastHTTPUserValueBifrostCancel, context.CancelFunc(func() {}))
		h.addMCPClient(rctx)
		return rctx.Response.StatusCode(), string(rctx.Response.Body())
	}

	t.Run("per_user_headers", func(t *testing.T) {
		h, recorder := newVerificationHandler(t, newVerificationRuntime(t))
		status, body := create(t, h, `{"name":"notes","connection_type":"http","connection_string":"http://upstream.invalid/mcp","auth_type":"per_user_headers",`+
			`"per_user_header_keys":["x-api-key"],"user_headers":{"x-api-key":"sample"},"tools_to_execute":["*"],"per_user_discovery":true}`, nil)
		require.Equal(t, fasthttp.StatusOK, status, body)

		require.Len(t, recorder.added, 1)
		assert.Equal(t, schemas.Ptr(true), recorder.added[0].PerUserDiscovery)
		assert.Nil(t, recorder.added[0].DiscoveredTools)
		stored, err := h.store.ConfigStore.GetMCPClientConfigByID(ctx, recorder.added[0].ID)
		require.NoError(t, err)
		assert.Equal(t, schemas.Ptr(true), stored.PerUserDiscovery)
		assert.Nil(t, stored.DiscoveredTools)
		assert.False(t, stored.PendingVerification, "verified")
	})

	t.Run("token_exchange", func(t *testing.T) {
		h, recorder := newVerificationHandler(t, newVerificationRuntime(t))
		status, body := create(t, h, `{"name":"jira","connection_type":"http","connection_string":"http://upstream.invalid/mcp","auth_type":"token_exchange",`+
			`"token_exchange":{"audience":"api://jira","client_id":"exchange-app"},"tools_to_execute":["*"],"per_user_discovery":true}`,
			map[schemas.BifrostContextKey]any{schemas.BifrostContextKeyIsEnterprise: true, schemas.BifrostContextKeyMCPInboundBearer: "admin-idp-token"})
		require.Equal(t, fasthttp.StatusOK, status, body)

		require.Len(t, recorder.added, 1)
		assert.Nil(t, recorder.added[0].DiscoveredTools)
		stored, err := h.store.ConfigStore.GetMCPClientConfigByID(ctx, recorder.added[0].ID)
		require.NoError(t, err)
		assert.Equal(t, schemas.Ptr(true), stored.PerUserDiscovery)
		assert.Nil(t, stored.DiscoveredTools)
		assert.False(t, stored.PendingVerification, "verified")
		admin, err := h.store.ConfigStore.GetAdminOauthTokenByMCPClientID(ctx, recorder.added[0].ID)
		require.NoError(t, err)
		assert.Nil(t, admin, "the admin's exchanged token is not kept")
	})

	t.Run("per_user_oauth", func(t *testing.T) {
		h, _ := newVerificationHandler(t, newVerificationRuntime(t))
		status, body := create(t, h, `{"name":"wiki","connection_type":"http","connection_string":"http://upstream.invalid/mcp","auth_type":"per_user_oauth",`+
			`"oauth_config":{"client_id":"bifrost","authorize_url":"https://idp.invalid/authorize","token_url":"https://idp.invalid/token"},"tools_to_execute":["*"],"per_user_discovery":true}`, nil)
		require.Equal(t, fasthttp.StatusOK, status, body)

		// Created once the admin's sign-in completes, from the config kept until then.
		var started struct {
			OauthConfigID string `json:"oauth_config_id"`
		}
		require.NoError(t, json.Unmarshal([]byte(body), &started))
		pending, err := h.oauthHandler.GetPendingMCPClient(started.OauthConfigID)
		require.NoError(t, err)
		require.NotNil(t, pending)
		assert.Equal(t, schemas.Ptr(true), pending.PerUserDiscovery)
	})

	t.Run("refused for a shared auth type", func(t *testing.T) {
		h, recorder := newVerificationHandler(t, newVerificationRuntime(t))
		status, body := create(t, h, `{"name":"notes","connection_type":"http","connection_string":"http://upstream.invalid/mcp","auth_type":"headers",`+
			`"tools_to_execute":["*"],"per_user_discovery":true}`, nil)
		assert.Equal(t, fasthttp.StatusBadRequest, status, body)
		assert.Contains(t, body, "per_user_discovery is only valid for per-user auth types")
		assert.Empty(t, recorder.added)
	})
}
