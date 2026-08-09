package awsmcproxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// whoami is what the fake AWS MCP Server reports back: enough to tell which AWS
// identity signed the request and what metadata rode along with it.
type whoami struct {
	Authorization string            `json:"authorization"`
	Meta          map[string]string `json:"meta"`
	Echo          string            `json:"echo"`
}

// newFakeAWSMCPServer starts an MCP server over streamable HTTP that mirrors
// back the SigV4 identity and the _meta of each call, so tests can assert what
// the proxy actually put on the wire.
func newFakeAWSMCPServer(t *testing.T, configure ...func(*mcp.Server)) *httptest.Server {
	t.Helper()

	return newFakeAWSMCPServerWithOptions(t, nil, configure...)
}

// newFakeAWSMCPServerWithOptions is newFakeAWSMCPServer with control over the
// server options and a hook to install middleware.
func newFakeAWSMCPServerWithOptions(t *testing.T, options *mcp.ServerOptions, configure ...func(*mcp.Server)) *httptest.Server {
	t.Helper()

	server := mcp.NewServer(&mcp.Implementation{Name: "fake-aws-mcp", Version: "0"}, options)

	server.AddTool(
		&mcp.Tool{
			Name:        "whoami",
			Description: "Echo the caller's identity.",
			InputSchema: map[string]any{
				"type":       "object",
				"properties": map[string]any{"echo": map[string]any{"type": "string"}},
			},
		},
		func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			var args struct {
				Echo string `json:"echo"`
			}

			if len(req.Params.Arguments) > 0 {
				require.NoError(t, json.Unmarshal(req.Params.Arguments, &args))
			}

			meta := map[string]string{}

			for key, value := range req.Params.Meta {
				meta[key] = fmt.Sprint(value)
			}

			buf, err := json.Marshal(whoami{
				Authorization: req.Extra.Header.Get("Authorization"),
				Meta:          meta,
				Echo:          args.Echo,
			})
			require.NoError(t, err)

			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: string(buf)}},
			}, nil
		},
	)

	for _, configure := range configure {
		configure(server)
	}

	httpServer := httptest.NewServer(serveMCP(server))
	t.Cleanup(httpServer.Close)

	return httpServer
}

func serveMCP(server *mcp.Server) http.Handler {
	return mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil)
}

// gate holds an upstream request open on demand, so a test can act while a
// connection is still being established.
type gate struct {
	mu      sync.Mutex
	release chan struct{}
	entered chan struct{}
}

// arm makes the next request block until open is called.
func (g *gate) arm() {
	g.mu.Lock()
	defer g.mu.Unlock()

	g.release = make(chan struct{})
	g.entered = make(chan struct{}, 1)
}

func (g *gate) hold() {
	g.mu.Lock()
	release, entered := g.release, g.entered
	g.mu.Unlock()

	if release == nil {
		return
	}

	select {
	case entered <- struct{}{}:
	default:
	}

	<-release
}

// waitEntered blocks until a request has reached the armed gate.
func (g *gate) waitEntered(t *testing.T) {
	t.Helper()

	select {
	case <-g.entered:
	case <-time.After(30 * time.Second):
		t.Fatal("no request reached the gate")
	}
}

func (g *gate) open() {
	g.mu.Lock()
	release := g.release
	g.release = nil
	g.mu.Unlock()

	if release != nil {
		close(release)
	}
}

func newGatedFakeAWSMCPServer(t *testing.T) (*httptest.Server, *gate) {
	t.Helper()

	held := &gate{}
	server := mcp.NewServer(&mcp.Implementation{Name: "fake-aws-mcp", Version: "0"}, nil)
	server.AddTool(
		&mcp.Tool{Name: "whoami", InputSchema: map[string]any{"type": "object"}},
		func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{}, nil
		},
	)

	handler := serveMCP(server)

	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		held.hold()
		handler.ServeHTTP(w, req)
	}))
	t.Cleanup(httpServer.Close)

	return httpServer, held
}

// failListTools makes the fake server reject tools/list, so the proxy's
// discovery path can be exercised against a server that connects but will not
// answer.
func failListTools(server *mcp.Server) {
	server.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			if method == "tools/list" {
				return nil, errors.New("tools/list is broken")
			}

			return next(ctx, method, req)
		}
	})
}

// brokenSchema makes the fake server advertise a tool whose input schema is not
// a JSON object, which the proxy cannot inject its profile argument into.
func brokenSchema(server *mcp.Server) {
	server.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			result, err := next(ctx, method, req)

			if method != "tools/list" || err != nil {
				return result, err
			}

			tools, ok := result.(*mcp.ListToolsResult)

			if !ok {
				return result, nil
			}

			for _, tool := range tools.Tools {
				tool.InputSchema = []any{"not an object"}
			}

			return tools, nil
		}
	})
}

// setupAWSProfiles points the AWS credential chain at throwaway shared files so
// each profile signs with a distinguishable access key.
func setupAWSProfiles(t *testing.T) {
	t.Helper()

	useEmptySharedFiles(t)
	writeSharedFile(t, awsSharedConfigFileEnv, "config", `
[profile dev]
region = us-east-1

[profile prod]
region = ap-northeast-1
`)
	writeSharedFile(t, awsSharedCredentialsFileEnv, "credentials", `
[dev]
aws_access_key_id = AKIADEV
aws_secret_access_key = devsecret

[prod]
aws_access_key_id = AKIAPROD
aws_secret_access_key = prodsecret
`)

	// Keep the developer's own environment out of the test.
	t.Setenv("AWS_PROFILE", "dev")
	t.Setenv("AWS_ACCESS_KEY_ID", "")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "")
	t.Setenv("AWS_SESSION_TOKEN", "")
	t.Setenv("AWS_REGION", "")
	t.Setenv("AWS_DEFAULT_REGION", "")
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
}

func testProxy(t *testing.T, endpoint string) *Proxy {
	t.Helper()

	// Built directly rather than through NewProxy: httptest serves on
	// 127.0.0.1, which identifies neither the signing service nor the region,
	// and setting AWS_REGION to supply them would also override each profile's
	// own region.
	proxy := &Proxy{
		endpoint: endpoint,
		service:  "aws-mcp",
		region:   "us-east-1",
		version:  "test",
		baseCtx:  context.Background(),
		sessions: map[string]*mcp.ClientSession{},
	}
	t.Cleanup(proxy.closeSessions)

	return proxy
}

// newTestSession builds a proxy in front of a fake AWS MCP Server and returns a
// client session connected to it, as an MCP client would be over stdio.
func newTestSession(t *testing.T, proxy *Proxy) *mcp.ClientSession {
	t.Helper()

	ctx := context.Background()

	server, err := proxy.buildServer(ctx)
	require.NoError(t, err)

	serverTransport, clientTransport := mcp.NewInMemoryTransports()

	serverSession, err := server.Connect(ctx, serverTransport, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = serverSession.Close() })

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0"}, nil)
	session, err := client.Connect(ctx, clientTransport, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = session.Close() })

	return session
}

func callWhoami(t *testing.T, session *mcp.ClientSession, args map[string]any) *mcp.CallToolResult {
	t.Helper()

	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "whoami", Arguments: args})
	require.NoError(t, err)

	return result
}

func decodeWhoami(t *testing.T, result *mcp.CallToolResult) whoami {
	t.Helper()
	require.False(t, result.IsError, resultText(result))
	require.Len(t, result.Content, 1)

	var decoded whoami
	require.NoError(t, json.Unmarshal([]byte(resultText(result)), &decoded))

	return decoded
}

func resultText(result *mcp.CallToolResult) string {
	var text strings.Builder

	for _, content := range result.Content {
		if textContent, ok := content.(*mcp.TextContent); ok {
			text.WriteString(textContent.Text)
		}
	}

	return text.String()
}

func TestNewProxyDefaultEndpoint(t *testing.T) {
	proxy, err := NewProxy(&Options{}, "test")
	require.NoError(t, err)

	assert.Equal(t, DefaultEndpoint, proxy.endpoint)
	assert.Equal(t, "aws-mcp", proxy.service)
	assert.Equal(t, "us-east-1", proxy.region)
}

func TestNewProxyEndpointError(t *testing.T) {
	_, err := NewProxy(&Options{Endpoint: "http://aws-mcp.us-east-1.api.aws/mcp"}, "test")
	require.Error(t, err)

	assert.Contains(t, err.Error(), "must be sent over HTTPS")
}

func TestProxyMirrorsTools(t *testing.T) {
	setupAWSProfiles(t)
	upstream := newFakeAWSMCPServer(t)
	session := newTestSession(t, testProxy(t, upstream.URL))

	result, err := session.ListTools(context.Background(), nil)
	require.NoError(t, err)

	names := make([]string, len(result.Tools))

	for i, tool := range result.Tools {
		names[i] = tool.Name
	}

	assert.ElementsMatch(t, []string{"list_profiles", "whoami"}, names)

	for _, tool := range result.Tools {
		if tool.Name != "whoami" {
			continue
		}

		schema, ok := tool.InputSchema.(map[string]any)
		require.True(t, ok)

		properties, ok := schema["properties"].(map[string]any)
		require.True(t, ok)

		// The upstream property survives alongside the injected one.
		assert.Contains(t, properties, "echo")
		assert.Contains(t, properties, profileArg)

		assert.Equal(t, []any{profileArg}, schema["required"])
	}
}

func TestProxyRoutesToProfile(t *testing.T) {
	setupAWSProfiles(t)
	upstream := newFakeAWSMCPServer(t)
	session := newTestSession(t, testProxy(t, upstream.URL))

	dev := decodeWhoami(t, callWhoami(t, session, map[string]any{profileArg: "dev", "echo": "hello"}))
	assert.Contains(t, dev.Authorization, "Credential=AKIADEV/")
	assert.Contains(t, dev.Authorization, "/us-east-1/aws-mcp/aws4_request")
	assert.Equal(t, "hello", dev.Echo)

	prod := decodeWhoami(t, callWhoami(t, session, map[string]any{profileArg: "prod"}))
	assert.Contains(t, prod.Authorization, "Credential=AKIAPROD/")
	// The endpoint region signs the request, whichever profile was used.
	assert.Contains(t, prod.Authorization, "/us-east-1/aws-mcp/aws4_request")
}

// TestProxySendsProfileRegion checks that the region the server should operate
// in comes from the profile's own configuration.
func TestProxySendsProfileRegion(t *testing.T) {
	setupAWSProfiles(t)
	upstream := newFakeAWSMCPServer(t)
	session := newTestSession(t, testProxy(t, upstream.URL))

	dev := decodeWhoami(t, callWhoami(t, session, map[string]any{profileArg: "dev"}))
	assert.Equal(t, "us-east-1", dev.Meta[regionMetadataKey])

	prod := decodeWhoami(t, callWhoami(t, session, map[string]any{profileArg: "prod"}))
	assert.Equal(t, "ap-northeast-1", prod.Meta[regionMetadataKey])
}

// TestProxyFallsBackToSigningRegion covers a profile with no region of its own.
func TestProxyFallsBackToSigningRegion(t *testing.T) {
	setupAWSProfiles(t)
	writeSharedFile(t, awsSharedConfigFileEnv, "config", "[profile dev]\n")

	upstream := newFakeAWSMCPServer(t)
	session := newTestSession(t, testProxy(t, upstream.URL))

	dev := decodeWhoami(t, callWhoami(t, session, map[string]any{profileArg: "dev"}))
	assert.Equal(t, "us-east-1", dev.Meta[regionMetadataKey])
}

func TestProxyMissingProfileArgument(t *testing.T) {
	setupAWSProfiles(t)
	upstream := newFakeAWSMCPServer(t)
	session := newTestSession(t, testProxy(t, upstream.URL))

	result := callWhoami(t, session, map[string]any{"echo": "hello"})
	assert.True(t, result.IsError)
	assert.Contains(t, resultText(result), "missing required argument 'profile'")
	assert.Contains(t, resultText(result), "available profiles: dev, prod")
}

func TestProxyUnknownProfile(t *testing.T) {
	setupAWSProfiles(t)
	upstream := newFakeAWSMCPServer(t)
	session := newTestSession(t, testProxy(t, upstream.URL))

	result := callWhoami(t, session, map[string]any{profileArg: "nope"})
	assert.True(t, result.IsError)
	assert.Contains(t, resultText(result), "failed to load the AWS config for profile 'nope'")
	assert.Contains(t, resultText(result), "available profiles: dev, prod")
}

func TestProxyListProfiles(t *testing.T) {
	setupAWSProfiles(t)
	upstream := newFakeAWSMCPServer(t)
	session := newTestSession(t, testProxy(t, upstream.URL))

	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "list_profiles"})
	require.NoError(t, err)
	require.False(t, result.IsError, resultText(result))

	var decoded struct {
		Profiles []string `json:"profiles"`
	}
	require.NoError(t, json.Unmarshal([]byte(resultText(result)), &decoded))

	assert.Equal(t, []string{"dev", "prod"}, decoded.Profiles)
}

// TestProxyListProfilesIsLive checks that a profile added after startup is
// visible without restarting the proxy.
func TestProxyListProfilesIsLive(t *testing.T) {
	setupAWSProfiles(t)
	upstream := newFakeAWSMCPServer(t)
	session := newTestSession(t, testProxy(t, upstream.URL))

	writeSharedFile(t, awsSharedConfigFileEnv, "config", "[profile dev]\n[profile prod]\n[profile staging]\n")

	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "list_profiles"})
	require.NoError(t, err)

	assert.Contains(t, resultText(result), "staging")
}

func TestProxySessionIsCached(t *testing.T) {
	setupAWSProfiles(t)
	upstream := newFakeAWSMCPServer(t)
	proxy := testProxy(t, upstream.URL)

	first, err := proxy.session(context.Background(), "dev")
	require.NoError(t, err)

	second, err := proxy.session(context.Background(), "dev")
	require.NoError(t, err)

	assert.Same(t, first, second)

	// Dropping it forces the next call to reconnect, which is how the proxy
	// recovers from a broken connection or expired credentials.
	proxy.dropSession("dev")

	third, err := proxy.session(context.Background(), "dev")
	require.NoError(t, err)

	assert.NotSame(t, first, third)
}

func TestProxyMalformedArguments(t *testing.T) {
	setupAWSProfiles(t)
	upstream := newFakeAWSMCPServer(t)
	proxy := testProxy(t, upstream.URL)

	_, handler, err := proxy.wrapTool(&mcp.Tool{Name: "whoami"})
	require.NoError(t, err)

	result, err := handler(context.Background(), &mcp.CallToolRequest{
		Params: &mcp.CallToolParamsRaw{Name: "whoami", Arguments: json.RawMessage("not json")},
	})
	require.NoError(t, err)

	assert.True(t, result.IsError)
	assert.Contains(t, resultText(result), "failed to parse arguments")
}

func TestProxyUpstreamFailureDropsSession(t *testing.T) {
	setupAWSProfiles(t)
	upstream := newFakeAWSMCPServer(t)
	proxy := testProxy(t, upstream.URL)

	_, handler, err := proxy.wrapTool(&mcp.Tool{Name: "whoami"})
	require.NoError(t, err)

	upstreamSession, err := proxy.session(context.Background(), "dev")
	require.NoError(t, err)

	// Break the connection behind the proxy's back.
	require.NoError(t, upstreamSession.Close())

	result, err := handler(context.Background(), &mcp.CallToolRequest{
		Params: &mcp.CallToolParamsRaw{Name: "whoami", Arguments: json.RawMessage(`{"profile":"dev"}`)},
	})
	require.NoError(t, err)

	assert.True(t, result.IsError)
	assert.Contains(t, resultText(result), "failed to call 'whoami' for profile 'dev'")

	// The broken session must not be handed out again.
	reconnected, err := proxy.session(context.Background(), "dev")
	require.NoError(t, err)
	assert.NotSame(t, upstreamSession, reconnected)
}

// TestProxyDiscoversToolsWithoutDefaultProfile covers the fallback that tries
// each profile in turn when the default credential chain cannot connect.
func TestProxyDiscoversToolsWithoutDefaultProfile(t *testing.T) {
	setupAWSProfiles(t)
	// No default profile and no credentials in the environment, so the default
	// chain has nothing to sign with.
	t.Setenv("AWS_PROFILE", "")

	upstream := newFakeAWSMCPServer(t)
	session := newTestSession(t, testProxy(t, upstream.URL))

	result, err := session.ListTools(context.Background(), nil)
	require.NoError(t, err)

	assert.Len(t, result.Tools, 2)
}

func TestProxyDiscoverToolsError(t *testing.T) {
	setupAWSProfiles(t)

	proxy := testProxy(t, "http://127.0.0.1:1/mcp")
	_, err := proxy.buildServer(context.Background())
	require.Error(t, err)

	assert.Contains(t, err.Error(), "failed to connect to the AWS MCP Server")
	// Every candidate identity is reported, not just the first.
	assert.Contains(t, err.Error(), "the default credential chain")
	assert.Contains(t, err.Error(), "profile 'prod'")
}

func TestProxyRunError(t *testing.T) {
	setupAWSProfiles(t)

	proxy := testProxy(t, "http://127.0.0.1:1/mcp")
	err := proxy.Run(context.Background())
	require.Error(t, err)

	assert.Contains(t, err.Error(), "failed to connect to the AWS MCP Server")
}

func TestProxyDiscoverToolsProfilesError(t *testing.T) {
	setupAWSProfiles(t)
	t.Setenv(awsSharedConfigFileEnv, notADirectory)

	upstream := newFakeAWSMCPServer(t)

	_, err := testProxy(t, upstream.URL).buildServer(context.Background())
	require.Error(t, err)

	assert.Contains(t, err.Error(), "failed to read")
}

func TestProxyListToolsError(t *testing.T) {
	setupAWSProfiles(t)
	upstream := newFakeAWSMCPServer(t, failListTools)

	_, err := testProxy(t, upstream.URL).buildServer(context.Background())
	require.Error(t, err)

	assert.Contains(t, err.Error(), "failed to list the AWS MCP Server tools")
}

func TestProxyWrapToolError(t *testing.T) {
	setupAWSProfiles(t)
	upstream := newFakeAWSMCPServer(t, brokenSchema)

	_, err := testProxy(t, upstream.URL).buildServer(context.Background())
	require.Error(t, err)

	assert.Contains(t, err.Error(), "failed to wrap tool 'whoami'")
}

// TestProxyFollowsPagination checks that a tool list split across pages is
// collected in full.
func TestProxyFollowsPagination(t *testing.T) {
	setupAWSProfiles(t)

	upstream := newFakeAWSMCPServerWithOptions(t, &mcp.ServerOptions{PageSize: 1}, func(server *mcp.Server) {
		server.AddTool(
			&mcp.Tool{Name: "second", InputSchema: map[string]any{"type": "object"}},
			func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				return &mcp.CallToolResult{}, nil
			},
		)
	})

	session := newTestSession(t, testProxy(t, upstream.URL))

	result, err := session.ListTools(context.Background(), nil)
	require.NoError(t, err)

	names := make([]string, len(result.Tools))

	for i, tool := range result.Tools {
		names[i] = tool.Name
	}

	// Both upstream tools survived the paginated listing, plus list_profiles.
	assert.ElementsMatch(t, []string{"list_profiles", "second", "whoami"}, names)
}

func TestProxyCancelledCall(t *testing.T) {
	setupAWSProfiles(t)
	upstream := newFakeAWSMCPServer(t)
	proxy := testProxy(t, upstream.URL)

	_, handler, err := proxy.wrapTool(&mcp.Tool{Name: "whoami"})
	require.NoError(t, err)

	// Warm the session, so the cancellation lands on the call itself.
	_, err = proxy.session(context.Background(), "dev")
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	result, err := handler(ctx, &mcp.CallToolRequest{
		Params: &mcp.CallToolParamsRaw{Name: "whoami", Arguments: json.RawMessage(`{"profile":"dev"}`)},
	})
	require.NoError(t, err)

	assert.True(t, result.IsError)
	assert.Contains(t, resultText(result), "was cancelled")

	// A cancelled call must not throw away a healthy session.
	proxy.mu.Lock()
	_, cached := proxy.sessions["dev"]
	proxy.mu.Unlock()
	assert.True(t, cached)
}

func TestConnectCtxWithoutBaseCtx(t *testing.T) {
	proxy := &Proxy{}
	ctx := context.Background()

	assert.Equal(t, ctx, proxy.connectCtx(ctx))

	proxy.baseCtx = context.TODO()
	assert.Equal(t, context.TODO(), proxy.connectCtx(ctx))
}

// TestProxyRunServesUntilClientDisconnects drives Run over the real stdio
// transport. Under `go test` stdin is /dev/null, so the client side is at EOF
// straight away and Run returns once it has built and served the server.
func TestProxyRunServesUntilClientDisconnects(t *testing.T) {
	setupAWSProfiles(t)
	upstream := newFakeAWSMCPServer(t)

	proxy := testProxy(t, upstream.URL)
	proxy.baseCtx = nil

	done := make(chan error, 1)

	go func() { done <- proxy.Run(context.Background()) }()

	select {
	case err := <-done:
		// EOF on stdin is a clean shutdown, not a failure.
		require.NoError(t, err)
	case <-time.After(30 * time.Second):
		t.Fatal("Run did not return after the stdio client disconnected")
	}
}

// TestProxySessionRace covers the branch where two callers open a connection
// for the same profile at once: the first session published wins, and the other
// is closed rather than replacing it.
func TestProxySessionRace(t *testing.T) {
	setupAWSProfiles(t)
	upstream, held := newGatedFakeAWSMCPServer(t)
	proxy := testProxy(t, upstream.URL)

	// A connection that is not in the cache yet, opened before the gate closes.
	existing, err := proxy.connect(context.Background(), "dev")
	require.NoError(t, err)

	// Hold the next connect open.
	held.arm()

	result := make(chan *mcp.ClientSession, 1)

	go func() {
		session, err := proxy.session(context.Background(), "dev")
		assert.NoError(t, err)
		result <- session
	}()

	held.waitEntered(t)

	// Publish a session for the same profile while the other connect is still
	// in flight, exactly as a racing caller would.
	proxy.mu.Lock()
	proxy.sessions["dev"] = existing
	proxy.mu.Unlock()

	held.open()

	select {
	case session := <-result:
		assert.Same(t, existing, session)
	case <-time.After(30 * time.Second):
		t.Fatal("session did not return")
	}
}
