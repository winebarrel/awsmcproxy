package awsmcproxy

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// profileArg is the name of the argument injected into every proxied tool to
// select the target AWS profile.
const profileArg = "profile"

// listProfilesTool returns a proxy-native tool that lists the configured AWS
// profiles (names, AWS profile, region and endpoint only, never credentials).
// It lets a client discover the valid values for the injected "profile"
// argument.
func (proxy *Proxy) listProfilesTool() (*mcp.Tool, mcp.ToolHandler) {
	tool := &mcp.Tool{
		Name:        "list_profiles",
		Description: "List the AWS profiles configured in awsmcproxy. Use the returned names as the 'profile' argument of the other tools.",
		InputSchema: map[string]any{
			"type":                 "object",
			"properties":           map[string]any{},
			"additionalProperties": false,
		},
	}

	handler := func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		type profileInfo struct {
			Name       string `json:"name"`
			AWSProfile string `json:"aws_profile,omitempty"`
			Region     string `json:"region,omitempty"`
			Endpoint   string `json:"endpoint,omitempty"`
		}

		profiles := make([]profileInfo, len(proxy.config.Profiles))

		for i, profile := range proxy.config.Profiles {
			profiles[i] = profileInfo{
				Name:       profile.Name,
				AWSProfile: profile.AWSProfile,
				Region:     profile.Region,
				Endpoint:   profile.Endpoint,
			}
		}

		buf, err := json.Marshal(map[string]any{"profiles": profiles})

		if err != nil {
			return errorResult("failed to encode profiles: %s", err), nil
		}

		return &mcp.CallToolResult{
			Content: []mcp.Content{
				&mcp.TextContent{Text: string(buf)},
			},
		}, nil
	}

	return tool, handler
}

// wrapTool returns a copy of the upstream tool with the "profile" argument
// injected, together with a handler that forwards the call over the profile's
// connection to the AWS MCP Server.
func (proxy *Proxy) wrapTool(tool *mcp.Tool, profileNames []string) (*mcp.Tool, mcp.ToolHandler, error) {
	schema, err := injectProfileArg(tool.InputSchema, profileNames)

	if err != nil {
		return nil, nil, err
	}

	wrapped := *tool
	wrapped.InputSchema = schema
	// OutputSchema is passed through unchanged from the upstream tool.

	toolName := tool.Name

	handler := func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := map[string]any{}

		if len(req.Params.Arguments) > 0 {
			if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
				return errorResult("failed to parse arguments: %s", err), nil
			}
		}

		profile, ok := args[profileArg].(string)

		if !ok || profile == "" {
			return errorResult("missing required argument '%s'; must be one of: %s", profileArg, strings.Join(profileNames, ", ")), nil
		}

		delete(args, profileArg)

		session, err := proxy.session(ctx, profile)

		if err != nil {
			return errorResult("%s (available profiles: %s)", err, strings.Join(profileNames, ", ")), nil
		}

		result, err := session.CallTool(ctx, &mcp.CallToolParams{
			Name:      toolName,
			Arguments: args,
		})

		if err != nil {
			// A cancelled or timed-out request does not mean the connection is
			// broken: keep the cached session and report the cancellation plainly.
			if ctx.Err() != nil {
				return errorResult("call to '%s' for profile '%s' was cancelled: %s", toolName, profile, err), nil
			}

			// Otherwise assume the session may be broken and drop it so the next
			// call reconnects. This is also how expired credentials recover: the
			// new connection is signed with freshly retrieved ones.
			proxy.dropSession(profile)

			return errorResult("failed to call '%s' for profile '%s': %s", toolName, profile, err), nil
		}

		return result, nil
	}

	return &wrapped, handler, nil
}

// injectProfileArg returns a copy of the given JSON schema with a required
// "profile" string property (enumerated over profileNames) added.
func injectProfileArg(inputSchema any, profileNames []string) (map[string]any, error) {
	schema := map[string]any{}

	// InputSchema arrives as a map[string]any from the upstream client, but
	// round-trip through JSON to get an independent, mutable copy regardless of
	// the concrete type.
	if inputSchema != nil {
		buf, err := json.Marshal(inputSchema)

		if err != nil {
			return nil, err
		}

		if err := json.Unmarshal(buf, &schema); err != nil {
			return nil, err
		}
	}

	if schema["type"] == nil {
		schema["type"] = "object"
	}

	properties, ok := schema["properties"].(map[string]any)

	if !ok {
		properties = map[string]any{}
		schema["properties"] = properties
	}

	enum := make([]any, len(profileNames))

	for i, name := range profileNames {
		enum[i] = name
	}

	properties[profileArg] = map[string]any{
		"type":        "string",
		"enum":        enum,
		"description": "The AWS profile to run this tool against. One of: " + strings.Join(profileNames, ", ") + ".",
	}

	schema["required"] = prependRequired(schema["required"], profileArg)

	return schema, nil
}

// prependRequired adds name to the front of a JSON schema "required" list,
// avoiding duplicates.
func prependRequired(existing any, name string) []any {
	required := []any{name}

	if list, ok := existing.([]any); ok {
		for _, item := range list {
			if item != name {
				required = append(required, item)
			}
		}
	}

	return required
}

// dropSession removes a cached upstream session so the next call reconnects.
func (proxy *Proxy) dropSession(profile string) {
	proxy.mu.Lock()
	session, ok := proxy.sessions[profile]
	delete(proxy.sessions, profile)
	proxy.mu.Unlock()

	if ok {
		_ = session.Close()
	}
}

// closeSessions closes and clears every cached upstream session.
func (proxy *Proxy) closeSessions() {
	proxy.mu.Lock()
	sessions := proxy.sessions
	proxy.sessions = map[string]*mcp.ClientSession{}
	proxy.mu.Unlock()

	for _, session := range sessions {
		_ = session.Close()
	}
}

func errorResult(format string, args ...any) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		IsError: true,
		Content: []mcp.Content{
			&mcp.TextContent{Text: fmt.Sprintf(format, args...)},
		},
	}
}
