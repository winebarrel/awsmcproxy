package awsmcproxy

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseEndpoint(t *testing.T) {
	tests := []struct {
		endpoint string
		service  string
		region   string
	}{
		{"https://aws-mcp.us-east-1.api.aws/mcp", "aws-mcp", "us-east-1"},
		{"https://aws-mcp.eu-central-1.api.aws/mcp", "aws-mcp", "eu-central-1"},
		{"https://my-gateway.bedrock-agentcore.ap-northeast-1.amazonaws.com/mcp", "bedrock-agentcore", "ap-northeast-1"},
		{"https://bedrock-agentcore.us-west-2.amazonaws.com/mcp", "bedrock-agentcore", "us-west-2"},
		// Not a recognized shape: the service falls back to the first label and
		// the region is left to the config.
		{"https://mcp.example.com/mcp", "mcp", ""},
		// Plain HTTP is allowed for local development only.
		{"http://localhost:8080/mcp", "localhost", ""},
		{"http://127.0.0.1:8080/mcp", "127", ""},
	}

	for _, tt := range tests {
		t.Run(tt.endpoint, func(t *testing.T) {
			service, region, err := parseEndpoint(tt.endpoint)
			require.NoError(t, err)

			assert.Equal(t, tt.service, service)
			assert.Equal(t, tt.region, region)
		})
	}
}

func TestParseEndpointError(t *testing.T) {
	tests := []struct {
		endpoint string
		errMsg   string
	}{
		{"http://aws-mcp.us-east-1.api.aws/mcp", "AWS credentials must be sent over HTTPS"},
		{"aws-mcp.us-east-1.api.aws/mcp", "missing URL scheme"},
		{"ftp://example.com/mcp", "unsupported scheme 'ftp'"},
		{"https:///mcp", "missing hostname"},
		{"https://exam ple.com/mcp", "invalid endpoint URL"},
	}

	for _, tt := range tests {
		t.Run(tt.endpoint, func(t *testing.T) {
			_, _, err := parseEndpoint(tt.endpoint)
			require.Error(t, err)

			assert.Contains(t, err.Error(), tt.errMsg)
		})
	}
}

func TestResolveSigning(t *testing.T) {
	service, region, err := resolveSigning("https://aws-mcp.eu-central-1.api.aws/mcp")
	require.NoError(t, err)

	assert.Equal(t, "aws-mcp", service)
	assert.Equal(t, "eu-central-1", region)
}

func TestResolveSigningRegionFromEnv(t *testing.T) {
	t.Setenv("AWS_REGION", "us-west-2")

	service, region, err := resolveSigning("https://mcp.example.com/mcp")
	require.NoError(t, err)

	assert.Equal(t, "mcp", service)
	assert.Equal(t, "us-west-2", region)
}

func TestResolveSigningError(t *testing.T) {
	t.Setenv("AWS_REGION", "")

	_, _, err := resolveSigning("https://mcp.example.com/mcp")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "could not determine the SigV4 service and region")

	_, _, err = resolveSigning("ftp://example.com/mcp")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported scheme")
}
