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

// listProfilesTool returns a proxy-native tool that lists the AWS profile
// names, and nothing else: a profile name is the whole identity of a profile
// in the shared AWS config. It lets a client discover the valid values for the
// injected "profile" argument.
//
// The list is read on every call, so a profile added to the shared config is
// usable without restarting the proxy.
func listProfilesTool() (*mcp.Tool, mcp.ToolHandler) {
	tool := &mcp.Tool{
		Name:        "list_profiles",
		Description: "List the available AWS profiles. Use the returned names as the 'profile' argument of the other tools.",
		InputSchema: map[string]any{
			"type":                 "object",
			"properties":           map[string]any{},
			"additionalProperties": false,
		},
	}

	handler := func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		profiles, err := Profiles()

		if err != nil {
			return errorResult("failed to list profiles: %s", err), nil
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
func (proxy *Proxy) wrapTool(tool *mcp.Tool) (*mcp.Tool, mcp.ToolHandler, error) {
	schema, err := injectProfileArg(tool.InputSchema)

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
			return errorResult("missing required argument '%s'%s", profileArg, availableProfiles()), nil
		}

		delete(args, profileArg)

		session, err := proxy.session(ctx, profile)

		if err != nil {
			return errorResult("%s%s", err, availableProfiles()), nil
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

// availableProfiles renders the profile names as a parenthesised hint for an
// error message, or nothing if they cannot be read.
func availableProfiles() string {
	profiles, err := Profiles()

	if err != nil || len(profiles) == 0 {
		return ""
	}

	return " (available profiles: " + strings.Join(profiles, ", ") + ")"
}

// injectProfileArg returns a copy of the given JSON schema with a required
// "profile" string property added.
//
// The valid values are deliberately not enumerated in the schema: the profile
// list is read from the shared AWS config on each use, so freezing it into the
// schema at startup would go stale. Clients discover the names through the
// list_profiles tool instead.
func injectProfileArg(inputSchema any) (map[string]any, error) {
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

	properties[profileArg] = map[string]any{
		"type":        "string",
		"description": "The AWS profile to run this tool against. Call list_profiles to see the available names.",
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
