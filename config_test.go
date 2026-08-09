package awsmcproxy

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yml")
	require.NoError(t, os.WriteFile(path, []byte(content), 0600))

	return path
}

func TestLoadConfig(t *testing.T) {
	t.Setenv("TEST_REGION", "ap-northeast-1")

	path := writeConfig(t, `
endpoint: https://aws-mcp.eu-central-1.api.aws/mcp
metadata:
  COMMON: shared
profiles:
  - name: dev
    aws_profile: my-dev
    region: us-east-1
  - name: prod
    aws_profile: my-prod
    region: ${TEST_REGION}
    metadata:
      EXTRA: "1"
`)

	config, err := LoadConfig(path)
	require.NoError(t, err)

	assert.Equal(t, []string{"dev", "prod"}, config.ProfileNames())
	assert.Equal(t, "my-dev", config.Profile("dev").AWSProfile)

	// Env expansion.
	assert.Equal(t, "ap-northeast-1", config.Profile("prod").Region)

	// The top-level endpoint is copied into every profile that does not override it.
	assert.Equal(t, "https://aws-mcp.eu-central-1.api.aws/mcp", config.Profile("dev").Endpoint)

	assert.Nil(t, config.Profile("nope"))
}

func TestLoadConfigDefaultEndpoint(t *testing.T) {
	path := writeConfig(t, "profiles:\n  - name: dev\n")

	config, err := LoadConfig(path)
	require.NoError(t, err)

	assert.Equal(t, DefaultEndpoint, config.Endpoint)
	assert.Equal(t, DefaultEndpoint, config.Profile("dev").Endpoint)
}

func TestLoadConfigProfileEndpoint(t *testing.T) {
	path := writeConfig(t, `
profiles:
  - name: dev
  - name: eu
    endpoint: https://aws-mcp.eu-central-1.api.aws/mcp
`)

	config, err := LoadConfig(path)
	require.NoError(t, err)

	assert.Equal(t, DefaultEndpoint, config.Profile("dev").Endpoint)
	assert.Equal(t, "https://aws-mcp.eu-central-1.api.aws/mcp", config.Profile("eu").Endpoint)

	_, region, err := config.signing(config.Profile("eu"))
	require.NoError(t, err)
	assert.Equal(t, "eu-central-1", region)
}

func TestLoadConfigError(t *testing.T) {
	tests := []struct {
		name    string
		content string
		errMsg  string
	}{
		{"no profiles", "profiles: []\n", "no profiles are configured"},
		{"empty profile", "profiles:\n  -\n", "profiles[0]: is empty"},
		{"no name", "profiles:\n  - aws_profile: my-dev\n", "profiles[0]: 'name' is required"},
		{"duplicated", "profiles:\n  - name: dev\n  - name: dev\n", "profiles[1]: duplicated profile name: dev"},
		{"bad endpoint", "endpoint: http://aws-mcp.us-east-1.api.aws/mcp\nprofiles:\n  - name: dev\n", "must be sent over HTTPS"},
		{"broken yaml", "profiles: [\n", "failed to parse config file"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := LoadConfig(writeConfig(t, tt.content))
			require.Error(t, err)

			assert.Contains(t, err.Error(), tt.errMsg)
		})
	}
}

func TestLoadConfigMissingFile(t *testing.T) {
	_, err := LoadConfig(filepath.Join(t.TempDir(), "nope.yml"))
	require.Error(t, err)

	assert.Contains(t, err.Error(), "failed to read config file")
}

func TestSigning(t *testing.T) {
	config := &Config{Profiles: []*ProfileConfig{{Name: "dev"}}}
	require.NoError(t, config.validate())

	service, region, err := config.signing(config.Profile("dev"))
	require.NoError(t, err)

	assert.Equal(t, "aws-mcp", service)
	assert.Equal(t, "us-east-1", region)
}

func TestSigningOverride(t *testing.T) {
	config := &Config{
		Service:       "bedrock-agentcore",
		SigningRegion: "ap-northeast-1",
		Profiles:      []*ProfileConfig{{Name: "dev"}},
	}
	require.NoError(t, config.validate())

	service, region, err := config.signing(config.Profile("dev"))
	require.NoError(t, err)

	assert.Equal(t, "bedrock-agentcore", service)
	assert.Equal(t, "ap-northeast-1", region)
}

func TestSigningRegionFromEnv(t *testing.T) {
	t.Setenv("AWS_REGION", "us-west-2")

	config := &Config{
		Endpoint: "https://mcp.example.com/mcp",
		Profiles: []*ProfileConfig{{Name: "dev"}},
	}
	require.NoError(t, config.validate())

	_, region, err := config.signing(config.Profile("dev"))
	require.NoError(t, err)

	assert.Equal(t, "us-west-2", region)
}

func TestSigningRegionUnknown(t *testing.T) {
	t.Setenv("AWS_REGION", "")

	config := &Config{
		Endpoint: "https://mcp.example.com/mcp",
		Profiles: []*ProfileConfig{{Name: "dev"}},
	}

	err := config.validate()
	require.Error(t, err)

	assert.Contains(t, err.Error(), "could not determine the SigV4 service and region")
}

func TestMetadata(t *testing.T) {
	config := &Config{
		Metadata: map[string]string{"COMMON": "shared", "AWS_REGION": "us-east-2"},
		Profiles: []*ProfileConfig{
			{Name: "plain"},
			{Name: "dev", Region: "us-east-1"},
			{Name: "prod", Region: "ap-northeast-1", Metadata: map[string]string{"AWS_REGION": "eu-west-1", "EXTRA": "1"}},
		},
	}
	require.NoError(t, config.validate())

	// Global metadata only.
	assert.Equal(t,
		map[string]string{"COMMON": "shared", "AWS_REGION": "us-east-2"},
		config.metadata(config.Profile("plain")),
	)

	// The profile's region wins over the global AWS_REGION.
	assert.Equal(t,
		map[string]string{"COMMON": "shared", "AWS_REGION": "us-east-1"},
		config.metadata(config.Profile("dev")),
	)

	// The profile's own metadata wins over its region.
	assert.Equal(t,
		map[string]string{"COMMON": "shared", "AWS_REGION": "eu-west-1", "EXTRA": "1"},
		config.metadata(config.Profile("prod")),
	)
}

func TestValidateIsIdempotent(t *testing.T) {
	config := &Config{Profiles: []*ProfileConfig{{Name: "dev"}}}

	require.NoError(t, config.validate())
	require.NoError(t, config.validate())

	assert.Equal(t, DefaultEndpoint, config.Profile("dev").Endpoint)
}
