package awsmcproxy

import (
	"fmt"
	"maps"
	"os"

	"gopkg.in/yaml.v3"
)

// DefaultEndpoint is the AWS MCP Server endpoint used when the config does not
// set one.
// See https://docs.aws.amazon.com/agent-toolkit/latest/userguide/getting-started-aws-mcp-server.html
const DefaultEndpoint = "https://aws-mcp.us-east-1.api.aws/mcp"

// regionMetadataKey is the metadata key the AWS MCP Server reads to pick the
// default region for the AWS operations it runs.
const regionMetadataKey = "AWS_REGION"

// ProfileConfig selects the AWS identity used to sign requests for one profile.
//
// AWSProfile names a profile in the shared AWS config, so `aws login` and
// `aws sso login` sessions are picked up through the standard credential chain.
// Region is sent as the AWS_REGION metadata entry, which sets the default
// region for the AWS operations the server runs on our behalf.
type ProfileConfig struct {
	Name       string            `yaml:"name"`
	AWSProfile string            `yaml:"aws_profile,omitempty"`
	Region     string            `yaml:"region,omitempty"`
	Endpoint   string            `yaml:"endpoint,omitempty"`
	Metadata   map[string]string `yaml:"metadata,omitempty"`
}

// Config is the awsmcproxy configuration file.
type Config struct {
	// Endpoint is the default AWS MCP Server endpoint. Defaults to DefaultEndpoint.
	Endpoint string `yaml:"endpoint,omitempty"`
	// Service overrides the SigV4 signing service name, which is otherwise
	// inferred from the endpoint hostname.
	Service string `yaml:"service,omitempty"`
	// SigningRegion overrides the SigV4 signing region, which is otherwise
	// inferred from the endpoint hostname. Leave it unset when profiles use
	// endpoints in different regions.
	SigningRegion string `yaml:"signing_region,omitempty"`
	// Metadata is sent as MCP _meta entries on every request, for all profiles.
	Metadata map[string]string `yaml:"metadata,omitempty"`
	Profiles []*ProfileConfig  `yaml:"profiles"`
}

// LoadConfig reads and validates the config file at path.
//
// The file content is passed through os.ExpandEnv so that values can be
// referenced as ${ENV_VAR} instead of being written literally.
func LoadConfig(path string) (*Config, error) {
	buf, err := os.ReadFile(path)

	if err != nil {
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}

	var config Config

	if err := yaml.Unmarshal([]byte(os.ExpandEnv(string(buf))), &config); err != nil {
		return nil, fmt.Errorf("failed to parse config file: %w", err)
	}

	if err := config.validate(); err != nil {
		return nil, err
	}

	return &config, nil
}

// validate fills in defaults and rejects a config the proxy cannot serve. It is
// idempotent, so it can also run on a programmatically built Config.
func (config *Config) validate() error {
	if config.Endpoint == "" {
		config.Endpoint = DefaultEndpoint
	}

	if len(config.Profiles) == 0 {
		return fmt.Errorf("no profiles are configured")
	}

	seen := map[string]bool{}

	for i, profile := range config.Profiles {
		if profile == nil {
			return fmt.Errorf("profiles[%d]: is empty", i)
		}

		if profile.Name == "" {
			return fmt.Errorf("profiles[%d]: 'name' is required", i)
		}

		if seen[profile.Name] {
			return fmt.Errorf("profiles[%d]: duplicated profile name: %s", i, profile.Name)
		}

		seen[profile.Name] = true

		if profile.Endpoint == "" {
			profile.Endpoint = config.Endpoint
		}

		// Reject an unusable endpoint at load time rather than on the first tool
		// call that happens to use the profile.
		if _, _, err := config.signing(profile); err != nil {
			return fmt.Errorf("profiles[%d]: %w", i, err)
		}
	}

	return nil
}

// signing returns the SigV4 service name and region used to sign requests to
// the profile's endpoint.
func (config *Config) signing(profile *ProfileConfig) (string, string, error) {
	service, region, err := parseEndpoint(profile.Endpoint)

	if err != nil {
		return "", "", err
	}

	if config.Service != "" {
		service = config.Service
	}

	if config.SigningRegion != "" {
		region = config.SigningRegion
	}

	if region == "" {
		// The signing region must match the endpoint's region, so this is only a
		// fallback for endpoints the hostname does not identify.
		region = os.Getenv("AWS_REGION")
	}

	if service == "" || region == "" {
		return "", "", fmt.Errorf(
			"could not determine the SigV4 service and region from endpoint '%s'; set 'service' and 'signing_region'",
			profile.Endpoint,
		)
	}

	return service, region, nil
}

// metadata returns the MCP _meta entries sent with every request for the
// profile. More specific settings win: global metadata, then the profile's
// region, then the profile's own metadata.
func (config *Config) metadata(profile *ProfileConfig) map[string]string {
	metadata := map[string]string{}

	maps.Copy(metadata, config.Metadata)

	if profile.Region != "" {
		metadata[regionMetadataKey] = profile.Region
	}

	maps.Copy(metadata, profile.Metadata)

	return metadata
}

// ProfileNames returns the configured profile names in file order.
func (config *Config) ProfileNames() []string {
	names := make([]string, len(config.Profiles))

	for i, profile := range config.Profiles {
		names[i] = profile.Name
	}

	return names
}

// Profile returns the config for the named profile, or nil if it is not configured.
func (config *Config) Profile(name string) *ProfileConfig {
	for _, profile := range config.Profiles {
		if profile.Name == name {
			return profile
		}
	}

	return nil
}
