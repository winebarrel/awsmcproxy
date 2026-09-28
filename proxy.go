package awsmcproxy

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sync"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials/ssocreds"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	appName = "awsmcproxy"
)

// regionMetadataKey is the metadata key the AWS MCP Server reads to pick the
// default region for the AWS operations it runs.
const regionMetadataKey = "AWS_REGION"

// Proxy is a multi-profile MCP proxy in front of the AWS MCP Server.
//
// The AWS MCP Server signs each request with SigV4, so a connection carries one
// AWS identity. The proxy opens one connection per AWS profile, exposes the
// server's tools over stdio, and injects a required "profile" argument into
// each tool. On a tool call the profile selects the matching connection, and the
// call is forwarded over it.
//
// Everything the proxy needs beyond the endpoint comes from the environment and
// the shared AWS config, so there is nothing else to configure.
type Proxy struct {
	endpoint string
	service  string
	region   string
	ssoRole  string
	version  string

	// baseCtx bounds the lifetime of the upstream connections. It is the Run
	// context, so a connection lives until the proxy stops -- not until the tool
	// call that first opened it returns.
	baseCtx context.Context

	mu       sync.Mutex
	sessions map[string]*mcp.ClientSession

	// loaded is closed once the upstream tools are mirrored (or failed to be),
	// and loadErr holds the failure. Tool requests wait on loaded, so the proxy
	// can answer initialize before the slow upstream connection is up.
	loaded  chan struct{}
	loadErr error
}

// NewProxy creates a Proxy for the endpoint in options.
func NewProxy(options *Options, version string) (*Proxy, error) {
	endpoint := options.Endpoint

	if endpoint == "" {
		endpoint = DefaultEndpoint
	}

	service, region, err := resolveSigning(endpoint)

	if err != nil {
		return nil, err
	}

	return &Proxy{
		endpoint: endpoint,
		service:  service,
		region:   region,
		ssoRole:  options.SSORole,
		version:  version,
		sessions: map[string]*mcp.ClientSession{},
	}, nil
}

// Run serves the proxy over stdio until the client disconnects or ctx is
// cancelled.
//
// It starts serving before the upstream tools are known: connecting to the AWS
// MCP Server takes seconds, and a client that times out waiting for the
// initialize response may send initialize again, which the SDK rejects as a
// duplicate. The tools are mirrored in the background instead, and tool
// requests wait for them. If mirroring fails, the proxy keeps running and
// reports the failure on those requests rather than exiting.
func (proxy *Proxy) Run(ctx context.Context) error {
	// Bind the upstream connections to the proxy's lifetime, and close them when
	// the proxy stops (client disconnect or ctx cancellation).
	proxy.baseCtx = ctx
	defer proxy.closeSessions()

	server := proxy.newServer()

	go func() {
		if err := proxy.loadTools(ctx, server); err != nil {
			log.Printf("[%s] %s", appName, err)
		}
	}()

	return server.Run(ctx, &mcp.StdioTransport{})
}

// buildServer builds the proxy server and mirrors the upstream tools into it,
// returning once they are loaded. It does not start serving.
func (proxy *Proxy) buildServer(ctx context.Context) (*mcp.Server, error) {
	server := proxy.newServer()

	if err := proxy.loadTools(ctx, server); err != nil {
		return nil, err
	}

	return server, nil
}

// newServer returns a server holding only the proxy-native list_profiles tool.
// Tool requests it receives wait until loadTools has run.
func (proxy *Proxy) newServer() *mcp.Server {
	proxy.loaded = make(chan struct{})

	server := mcp.NewServer(&mcp.Implementation{
		Name:    appName,
		Version: proxy.version,
	}, nil)

	server.AddReceivingMiddleware(proxy.waitForTools)
	// Add a proxy-native tool so clients can discover the available profiles.
	server.AddTool(listProfilesTool())

	return server
}

// waitForTools holds tools/list and tools/call until the upstream tools are
// mirrored, so a client never sees a partial tool list.
func (proxy *Proxy) waitForTools(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		if method != "tools/list" && method != "tools/call" {
			return next(ctx, method, req)
		}

		select {
		case <-proxy.loaded:
		case <-ctx.Done():
			return nil, ctx.Err()
		}

		// list_profiles still works without the upstream tools, and helps to
		// diagnose the failure.
		if proxy.loadErr != nil && method == "tools/list" {
			return nil, proxy.loadErr
		}

		return next(ctx, method, req)
	}
}

// loadTools connects to the AWS MCP Server and mirrors its tools into server,
// each with an injected profile argument. It records the outcome for
// waitForTools.
func (proxy *Proxy) loadTools(ctx context.Context, server *mcp.Server) error {
	defer close(proxy.loaded)

	proxy.loadErr = proxy.addTools(ctx, server)

	return proxy.loadErr
}

func (proxy *Proxy) addTools(ctx context.Context, server *mcp.Server) error {
	tools, err := proxy.discoverTools(ctx)

	if err != nil {
		return err
	}

	for _, tool := range tools {
		wrapped, handler, err := proxy.wrapTool(tool)

		if err != nil {
			return fmt.Errorf("failed to wrap tool '%s': %w", tool.Name, err)
		}

		server.AddTool(wrapped, handler)
	}

	log.Printf("[%s] serving %d AWS MCP tools from %s", appName, len(tools), proxy.endpoint)

	return nil
}

// discoverTools lists the tools to mirror. Every profile reaches the same
// server, so any identity that connects will do: try the default credential
// chain first, then each profile in turn. The connection is closed once the
// tools are known; the connections that serve tool calls are opened per profile
// on demand.
func (proxy *Proxy) discoverTools(ctx context.Context) ([]*mcp.Tool, error) {
	profiles, err := Profiles()

	if err != nil {
		return nil, err
	}

	// An empty profile means the default credential chain, which honours
	// AWS_PROFILE and static credentials in the environment.
	var errs []error

	for _, profile := range append([]string{""}, profiles...) {
		session, err := proxy.connect(ctx, profile)

		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", describeProfile(profile), err))

			continue
		}

		tools, err := listTools(ctx, session)
		_ = session.Close()

		if err != nil {
			return nil, fmt.Errorf("failed to list the AWS MCP Server tools: %w", err)
		}

		return tools, nil
	}

	return nil, fmt.Errorf("failed to connect to the AWS MCP Server at %s: %w", proxy.endpoint, errors.Join(errs...))
}

// session returns a connected session for the profile, opening and caching its
// connection on first use.
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

	session, err := proxy.connect(ctx, profile)

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

// connect opens an unmanaged session to the AWS MCP Server signed with the
// profile's credentials. An empty profile uses the default credential chain.
func (proxy *Proxy) connect(ctx context.Context, profile string) (*mcp.ClientSession, error) {
	httpClient, err := proxy.httpClient(ctx, profile)

	if err != nil {
		return nil, err
	}

	client := mcp.NewClient(&mcp.Implementation{
		Name:    appName,
		Version: proxy.version,
	}, nil)

	transport := &mcp.StreamableClientTransport{
		Endpoint:   proxy.endpoint,
		HTTPClient: httpClient,
	}

	// Connect on the proxy's context: the session, including its standalone SSE
	// stream, has to outlive the tool call that opened it.
	return client.Connect(proxy.connectCtx(ctx), transport, nil)
}

// httpClient builds the SigV4-signing HTTP client for the profile.
//
// Credentials come from the standard AWS credential chain, so `aws login`,
// `aws sso login`, assumed roles and static keys all work with no
// awsmcproxy-specific configuration. The region the server should operate in
// comes from the same place, resolved the way the AWS CLI resolves it.
func (proxy *Proxy) httpClient(ctx context.Context, profile string) (*http.Client, error) {
	options := []func(*awsconfig.LoadOptions) error{
		// Only a fallback: a region configured for the profile still wins. Some
		// credential providers (SSO, `aws login`, AssumeRole) need a region to
		// call their own APIs.
		awsconfig.WithDefaultRegion(proxy.region),
	}

	if profile != "" {
		options = append(options, awsconfig.WithSharedConfigProfile(profile))
	}

	if proxy.ssoRole != "" {
		// Applied after ssocreds.New has taken sso_role_name from the profile,
		// so this wins. Only IAM Identity Center reaches here, and only a role
		// the SSO session is already entitled to will work -- an unassigned one
		// fails when the credentials are first retrieved, not now.
		options = append(options, awsconfig.WithSSOProviderOptions(func(o *ssocreds.Options) {
			o.RoleName = proxy.ssoRole
		}))
	}

	awsConfig, err := awsconfig.LoadDefaultConfig(ctx, options...)

	if err != nil {
		return nil, fmt.Errorf("failed to load the AWS config for %s: %w", describeProfile(profile), err)
	}

	metadata := map[string]string{regionMetadataKey: awsConfig.Region}
	transport := newSigningTransport(awsConfig.Credentials, proxy.service, proxy.region, metadata)

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

func describeProfile(profile string) string {
	if profile == "" {
		return "the default credential chain"
	}

	return fmt.Sprintf("profile '%s'", profile)
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
