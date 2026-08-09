package awsmcproxy

import (
	"fmt"
	"net/url"
	"os"
	"strings"
)

// parseEndpoint validates an MCP endpoint URL and infers the SigV4 signing
// service and region from its hostname.
//
// AWS credentials travel in the SigV4 headers, so the endpoint must be HTTPS.
// Plain HTTP is accepted for loopback hosts only, for local development.
//
// The inference mirrors mcp-proxy-for-aws:
//
//	*.bedrock-agentcore.<region>.amazonaws.com -> bedrock-agentcore, <region>
//	<service>.<region>.api.aws                 -> <service>, <region>
//	<service>.<anything else>                  -> <service>, ""
//
// A returned region may be empty; the caller decides how to fill it in.
func parseEndpoint(endpoint string) (string, string, error) {
	parsed, err := url.Parse(endpoint)

	if err != nil {
		return "", "", fmt.Errorf("invalid endpoint URL '%s': %w", endpoint, err)
	}

	switch {
	case parsed.Scheme == "https":
		// OK.
	case parsed.Scheme == "http" && isLoopback(parsed.Hostname()):
		// OK: local development.
	case parsed.Scheme == "http":
		return "", "", fmt.Errorf("invalid endpoint URL '%s': AWS credentials must be sent over HTTPS", endpoint)
	case parsed.Scheme == "":
		return "", "", fmt.Errorf("invalid endpoint URL '%s': missing URL scheme", endpoint)
	default:
		return "", "", fmt.Errorf("invalid endpoint URL '%s': unsupported scheme '%s'", endpoint, parsed.Scheme)
	}

	hostname := parsed.Hostname()

	if hostname == "" {
		return "", "", fmt.Errorf("invalid endpoint URL '%s': missing hostname", endpoint)
	}

	labels := strings.Split(hostname, ".")

	if n := len(labels); n >= 4 && labels[n-4] == "bedrock-agentcore" && labels[n-2] == "amazonaws" && labels[n-1] == "com" {
		return "bedrock-agentcore", labels[n-3], nil
	}

	if len(labels) == 4 && labels[2] == "api" && labels[3] == "aws" {
		return labels[0], labels[1], nil
	}

	return labels[0], "", nil
}

// resolveSigning returns the SigV4 service and region used to sign requests to
// the endpoint.
func resolveSigning(endpoint string) (string, string, error) {
	service, region, err := parseEndpoint(endpoint)

	if err != nil {
		return "", "", err
	}

	if region == "" {
		// The signing region has to match the endpoint's region, so this is
		// only a fallback for an endpoint the hostname does not identify.
		region = os.Getenv("AWS_REGION")
	}

	if service == "" || region == "" {
		return "", "", fmt.Errorf("could not determine the SigV4 service and region from endpoint '%s'", endpoint)
	}

	return service, region, nil
}

func isLoopback(hostname string) bool {
	switch hostname {
	case "localhost", "127.0.0.1", "::1":
		return true
	}

	return false
}
