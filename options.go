package awsmcproxy

// DefaultEndpoint is the AWS MCP Server endpoint used when none is given.
// See https://docs.aws.amazon.com/agent-toolkit/latest/userguide/getting-started-aws-mcp-server.html
const DefaultEndpoint = "https://aws-mcp.us-east-1.api.aws/mcp"

// Options holds the command-line options.
type Options struct {
	Endpoint string `kong:"short='e',env='AWSMCPROXY_ENDPOINT',default='${endpoint}',help='AWS MCP Server endpoint.'"`
	SSORole  string `kong:"name='sso-role',env='AWSMCPROXY_SSO_ROLE',help='Override sso_role_name for every profile, e.g. ReadOnlyAccess.'"`
}
