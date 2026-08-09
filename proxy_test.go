package awsmcproxy

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
func newFakeAWSMCPServer(t *testing.T) *httptest.Server {
	t.Helper()

	server := mcp.NewServer(&mcp.Implementation{Name: "fake-aws-mcp", Version: "0"}, nil)

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

	httpServer := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil))
	t.Cleanup(httpServer.Close)

	return httpServer
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
