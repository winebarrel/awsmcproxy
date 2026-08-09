package awsmcproxy

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"sync"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	appName = "awsmcproxy"
)

// Proxy is a multi-profile MCP proxy in front of the AWS MCP Server.
//
// The AWS MCP Server signs each request with SigV4, so a connection carries one
// AWS identity. The proxy opens one connection per configured profile, exposes
// the server's tools over stdio, and injects a required "profile" argument into
// each tool. On a tool call the profile selects the matching connection, and the
// call is forwarded over it.
type Proxy struct {
	config  *Config
	version string

	// baseCtx bounds the lifetime of the upstream connections. It is the Run
	// context, so a connection lives until the proxy stops -- not until the tool
	// call that first opened it returns.
	baseCtx context.Context

	mu       sync.Mutex
	sessions map[string]*mcp.ClientSession
}

// NewProxy creates a Proxy from the given config.
func NewProxy(config *Config, version string) *Proxy {
	return &Proxy{
		config:   config,
		version:  version,
		sessions: map[string]*mcp.ClientSession{},
	}
}

// Run builds the proxy server and serves it over stdio until the client
// disconnects or ctx is cancelled.
func (proxy *Proxy) Run(ctx context.Context) error {
	// Bind the upstream connections to the proxy's lifetime, and close them when
	// the proxy stops (client disconnect or ctx cancellation).
	proxy.baseCtx = ctx
	defer proxy.closeSessions()

	server, err := proxy.buildServer(ctx)

	if err != nil {
		return err
	}

	return server.Run(ctx, &mcp.StdioTransport{})
}

// buildServer connects to the AWS MCP Server as the first profile, mirrors its
// tools (each with an injected profile argument, plus a proxy-native
// list_profiles tool) and returns a server ready to serve. It does not start
// serving.
func (proxy *Proxy) buildServer(ctx context.Context) (*mcp.Server, error) {
	if proxy.config == nil {
		return nil, fmt.Errorf("no config is set")
	}

	// Validate here too so a programmatically constructed config (one that did
	// not go through LoadConfig) is rejected -- and defaults applied -- before
	// any profile entry is dereferenced.
	if err := proxy.config.validate(); err != nil {
		return nil, err
	}

	server := mcp.NewServer(&mcp.Implementation{
		Name:    appName,
		Version: proxy.version,
	}, nil)

	// Discover the tools using the first configured profile. Every profile is
	// assumed to reach a server exposing the same set of tools.
	primary := proxy.config.Profiles[0].Name
	session, err := proxy.session(ctx, primary)

	if err != nil {
		return nil, fmt.Errorf("failed to connect to the AWS MCP Server for profile '%s': %w", primary, err)
	}

	tools, err := listTools(ctx, session)

	if err != nil {
		return nil, fmt.Errorf("failed to list upstream tools: %w", err)
	}

	profileNames := proxy.config.ProfileNames()

	// Add a proxy-native tool so clients can discover the configured profiles.
	server.AddTool(proxy.listProfilesTool())

	for _, tool := range tools {
		wrapped, handler, err := proxy.wrapTool(tool, profileNames)

		if err != nil {
			return nil, fmt.Errorf("failed to wrap tool '%s': %w", tool.Name, err)
		}

		server.AddTool(wrapped, handler)
	}

	log.Printf("[%s] serving %d AWS MCP tools for %d profiles: %v", appName, len(tools), len(profileNames), profileNames)

	return server, nil
}

// session returns a connected upstream session for the profile, opening and
// caching its connection on first use.
//
// The connection is opened without holding proxy.mu so that a slow connect does
// not block tool calls for other profiles. If two callers race to open the same
// profile, both connect but only the first published session is kept; the
// loser's session is closed.
func (proxy *Proxy) session(ctx context.Context, profile string) (*mcp.ClientSession, error) {
	proxy.mu.Lock()
	cached, ok := proxy.sessions[profile]
	proxy.mu.Unlock()

	if ok {
		return cached, nil
	}

	profileConfig := proxy.config.Profile(profile)

	if profileConfig == nil {
		return nil, fmt.Errorf("unknown profile: %s", profile)
	}

	httpClient, err := proxy.httpClient(ctx, profileConfig)

	if err != nil {
		return nil, err
	}

	client := mcp.NewClient(&mcp.Implementation{
		Name:    appName,
		Version: proxy.version,
	}, nil)

	transport := &mcp.StreamableClientTransport{
		Endpoint:   profileConfig.Endpoint,
		HTTPClient: httpClient,
	}

	// Connect on the proxy's context: the session, including its standalone SSE
	// stream, has to outlive the tool call that opened it.
	session, err := client.Connect(proxy.connectCtx(ctx), transport, nil)

	if err != nil {
		return nil, err
	}

	proxy.mu.Lock()
	// Another goroutine may have opened the same profile while we were connecting.
	if existing, ok := proxy.sessions[profile]; ok {
		proxy.mu.Unlock()
		_ = session.Close()

		return existing, nil
	}

	proxy.sessions[profile] = session
	proxy.mu.Unlock()

	return session, nil
}

// httpClient builds the SigV4-signing HTTP client used to reach the profile's
// endpoint.
//
// Credentials come from the standard AWS credential chain for the profile, so
// `aws login`, `aws sso login`, assumed roles and static keys all work without
// any awsmcproxy-specific configuration.
func (proxy *Proxy) httpClient(ctx context.Context, profile *ProfileConfig) (*http.Client, error) {
	service, region, err := proxy.config.signing(profile)

	if err != nil {
		return nil, err
	}

	options := []func(*awsconfig.LoadOptions) error{
		// Only a fallback: a region configured for the profile still wins. Some
		// credential providers (SSO, `aws login`, AssumeRole) need a region to
		// call their own APIs.
		awsconfig.WithDefaultRegion(region),
	}

	if profile.AWSProfile != "" {
		options = append(options, awsconfig.WithSharedConfigProfile(profile.AWSProfile))
	}

	awsConfig, err := awsconfig.LoadDefaultConfig(ctx, options...)

	if err != nil {
		return nil, fmt.Errorf("failed to load the AWS config for profile '%s': %w", profile.Name, err)
	}

	transport := newSigningTransport(awsConfig.Credentials, service, region, proxy.config.metadata(profile))

	// No Client.Timeout: it would also cut off the long-lived SSE stream. Tool
	// calls are bounded by their own context instead.
	return &http.Client{Transport: transport}, nil
}

// connectCtx returns the context that bounds an upstream connection's lifetime.
func (proxy *Proxy) connectCtx(ctx context.Context) context.Context {
	if proxy.baseCtx != nil {
		return proxy.baseCtx
	}

	return ctx
}

// listTools collects every tool from the upstream server, following pagination.
func listTools(ctx context.Context, session *mcp.ClientSession) ([]*mcp.Tool, error) {
	var tools []*mcp.Tool

	params := &mcp.ListToolsParams{}

	for {
		result, err := session.ListTools(ctx, params)

		if err != nil {
			return nil, err
		}

		tools = append(tools, result.Tools...)

		if result.NextCursor == "" {
			break
		}

		params.Cursor = result.NextCursor
	}

	return tools, nil
}
