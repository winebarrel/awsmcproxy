package awsmcproxy

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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

// setupAWSProfiles points the AWS credential chain at a throwaway credentials
// file so each profile signs with a distinguishable access key.
func setupAWSProfiles(t *testing.T) {
	t.Helper()

	path := filepath.Join(t.TempDir(), "credentials")
	require.NoError(t, os.WriteFile(path, []byte(`
[my-dev]
aws_access_key_id = AKIADEV
aws_secret_access_key = devsecret

[my-prod]
aws_access_key_id = AKIAPROD
aws_secret_access_key = prodsecret
`), 0600))

	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", path)
	t.Setenv("AWS_CONFIG_FILE", filepath.Join(t.TempDir(), "config"))

	// Keep the developer's own environment out of the test.
	t.Setenv("AWS_PROFILE", "")
	t.Setenv("AWS_ACCESS_KEY_ID", "")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "")
	t.Setenv("AWS_SESSION_TOKEN", "")
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
}

// newTestProxy builds a proxy in front of a fake AWS MCP Server and returns a
// client session connected to it, as an MCP client would be over stdio.
func newTestProxy(t *testing.T, config *Config) *mcp.ClientSession {
	t.Helper()

	ctx := context.Background()
	proxy := NewProxy(config, "test")
	proxy.baseCtx = ctx
	t.Cleanup(proxy.closeSessions)

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

func testConfig(t *testing.T, endpoint string) *Config {
	t.Helper()

	return &Config{
		Endpoint: endpoint,
		// httptest serves on 127.0.0.1, which identifies neither.
		Service:       "aws-mcp",
		SigningRegion: "us-east-1",
		Profiles: []*ProfileConfig{
			{Name: "dev", AWSProfile: "my-dev", Region: "us-east-1"},
			{Name: "prod", AWSProfile: "my-prod", Region: "ap-northeast-1"},
		},
	}
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

func TestProxyMirrorsTools(t *testing.T) {
	setupAWSProfiles(t)
	upstream := newFakeAWSMCPServer(t)
	session := newTestProxy(t, testConfig(t, upstream.URL))

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

		profile, ok := properties[profileArg].(map[string]any)
		require.True(t, ok)
		assert.Equal(t, []any{"dev", "prod"}, profile["enum"])

		assert.Equal(t, []any{profileArg}, schema["required"])
	}
}

func TestProxyRoutesToProfile(t *testing.T) {
	setupAWSProfiles(t)
	upstream := newFakeAWSMCPServer(t)
	session := newTestProxy(t, testConfig(t, upstream.URL))

	dev := decodeWhoami(t, callWhoami(t, session, map[string]any{profileArg: "dev", "echo": "hello"}))
	assert.Contains(t, dev.Authorization, "Credential=AKIADEV/")
	assert.Contains(t, dev.Authorization, "/us-east-1/aws-mcp/aws4_request")
	assert.Equal(t, "us-east-1", dev.Meta["AWS_REGION"])
	assert.Equal(t, "hello", dev.Echo)

	prod := decodeWhoami(t, callWhoami(t, session, map[string]any{profileArg: "prod"}))
	assert.Contains(t, prod.Authorization, "Credential=AKIAPROD/")
	// The endpoint region signs the request; the profile region only tells the
	// server where to run the AWS operations.
	assert.Contains(t, prod.Authorization, "/us-east-1/aws-mcp/aws4_request")
	assert.Equal(t, "ap-northeast-1", prod.Meta["AWS_REGION"])
}

func TestProxyGlobalMetadata(t *testing.T) {
	setupAWSProfiles(t)
	upstream := newFakeAWSMCPServer(t)

	config := testConfig(t, upstream.URL)
	config.Metadata = map[string]string{"COMMON": "shared"}
	config.Profiles[0].Metadata = map[string]string{"EXTRA": "1"}

	session := newTestProxy(t, config)

	dev := decodeWhoami(t, callWhoami(t, session, map[string]any{profileArg: "dev"}))
	assert.Equal(t, "shared", dev.Meta["COMMON"])
	assert.Equal(t, "1", dev.Meta["EXTRA"])

	prod := decodeWhoami(t, callWhoami(t, session, map[string]any{profileArg: "prod"}))
	assert.Equal(t, "shared", prod.Meta["COMMON"])
	assert.NotContains(t, prod.Meta, "EXTRA")
}

func TestProxyMissingProfileArgument(t *testing.T) {
	setupAWSProfiles(t)
	upstream := newFakeAWSMCPServer(t)
	session := newTestProxy(t, testConfig(t, upstream.URL))

	result := callWhoami(t, session, map[string]any{"echo": "hello"})
	assert.True(t, result.IsError)
	assert.Contains(t, resultText(result), "missing required argument 'profile'")
}

func TestProxyUnknownProfile(t *testing.T) {
	setupAWSProfiles(t)
	upstream := newFakeAWSMCPServer(t)
	session := newTestProxy(t, testConfig(t, upstream.URL))

	result := callWhoami(t, session, map[string]any{profileArg: "nope"})
	assert.True(t, result.IsError)
	assert.Contains(t, resultText(result), "unknown profile: nope")
	assert.Contains(t, resultText(result), "available profiles: dev, prod")
}

func TestProxyListProfiles(t *testing.T) {
	setupAWSProfiles(t)
	upstream := newFakeAWSMCPServer(t)
	session := newTestProxy(t, testConfig(t, upstream.URL))

	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "list_profiles"})
	require.NoError(t, err)
	require.False(t, result.IsError, resultText(result))

	var decoded struct {
		Profiles []struct {
			Name       string `json:"name"`
			AWSProfile string `json:"aws_profile"`
			Region     string `json:"region"`
			Endpoint   string `json:"endpoint"`
		} `json:"profiles"`
	}
	require.NoError(t, json.Unmarshal([]byte(resultText(result)), &decoded))

	require.Len(t, decoded.Profiles, 2)
	assert.Equal(t, "dev", decoded.Profiles[0].Name)
	assert.Equal(t, "my-dev", decoded.Profiles[0].AWSProfile)
	assert.Equal(t, "us-east-1", decoded.Profiles[0].Region)
	assert.Equal(t, upstream.URL, decoded.Profiles[0].Endpoint)
	assert.Equal(t, "prod", decoded.Profiles[1].Name)
}

func TestProxySessionIsCached(t *testing.T) {
	setupAWSProfiles(t)
	upstream := newFakeAWSMCPServer(t)

	config := testConfig(t, upstream.URL)
	proxy := NewProxy(config, "test")
	proxy.baseCtx = context.Background()
	t.Cleanup(proxy.closeSessions)

	_, err := proxy.buildServer(context.Background())
	require.NoError(t, err)

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

func TestProxyUnknownProfileSession(t *testing.T) {
	setupAWSProfiles(t)
	upstream := newFakeAWSMCPServer(t)

	proxy := NewProxy(testConfig(t, upstream.URL), "test")
	require.NoError(t, proxy.config.validate())

	_, err := proxy.session(context.Background(), "nope")
	require.Error(t, err)

	assert.Contains(t, err.Error(), "unknown profile: nope")
}

func TestProxyBuildServerError(t *testing.T) {
	t.Run("no config", func(t *testing.T) {
		_, err := NewProxy(nil, "test").buildServer(context.Background())
		require.Error(t, err)

		assert.Contains(t, err.Error(), "no config is set")
	})

	t.Run("invalid config", func(t *testing.T) {
		_, err := NewProxy(&Config{}, "test").buildServer(context.Background())
		require.Error(t, err)

		assert.Contains(t, err.Error(), "no profiles are configured")
	})

	t.Run("unreachable endpoint", func(t *testing.T) {
		setupAWSProfiles(t)

		config := testConfig(t, "http://127.0.0.1:1/mcp")
		_, err := NewProxy(config, "test").buildServer(context.Background())
		require.Error(t, err)

		assert.Contains(t, err.Error(), "failed to connect to the AWS MCP Server for profile 'dev'")
	})

	t.Run("missing AWS profile", func(t *testing.T) {
		setupAWSProfiles(t)
		upstream := newFakeAWSMCPServer(t)

		config := testConfig(t, upstream.URL)
		config.Profiles[0].AWSProfile = "does-not-exist"

		_, err := NewProxy(config, "test").buildServer(context.Background())
		require.Error(t, err)

		assert.Contains(t, err.Error(), "failed to load the AWS config for profile 'dev'")
	})
}

func TestProxyMalformedArguments(t *testing.T) {
	setupAWSProfiles(t)
	upstream := newFakeAWSMCPServer(t)

	proxy := NewProxy(testConfig(t, upstream.URL), "test")
	proxy.baseCtx = context.Background()
	t.Cleanup(proxy.closeSessions)
	require.NoError(t, proxy.config.validate())

	_, handler, err := proxy.wrapTool(&mcp.Tool{Name: "whoami"}, proxy.config.ProfileNames())
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

	proxy := NewProxy(testConfig(t, upstream.URL), "test")
	proxy.baseCtx = context.Background()
	t.Cleanup(proxy.closeSessions)
	require.NoError(t, proxy.config.validate())

	_, handler, err := proxy.wrapTool(&mcp.Tool{Name: "whoami"}, proxy.config.ProfileNames())
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

func TestProxyRunError(t *testing.T) {
	err := NewProxy(&Config{}, "test").Run(context.Background())
	require.Error(t, err)

	assert.Contains(t, err.Error(), "no profiles are configured")
}
