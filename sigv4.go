package awsmcproxy

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
)

// metaField is the MCP field carrying request metadata. The AWS MCP Server
// reads AWS_REGION from it; mcp-proxy-for-aws passes metadata the same way
// because HTTP headers are too small for some values.
const metaField = "_meta"

// signingTransport signs every outgoing request with AWS SigV4, injecting the
// configured MCP metadata first so it is covered by the signature.
//
// Credentials are retrieved per request from Credentials, which is a caching
// provider, so a refreshed `aws login` or `aws sso login` session is picked up
// without restarting the proxy.
type signingTransport struct {
	Base        http.RoundTripper
	Credentials aws.CredentialsProvider
	Service     string
	Region      string
	Metadata    map[string]string

	signer *v4.Signer
}

func newSigningTransport(credentials aws.CredentialsProvider, service string, region string, metadata map[string]string) *signingTransport {
	return &signingTransport{
		Credentials: credentials,
		Service:     service,
		Region:      region,
		Metadata:    metadata,
		signer:      v4.NewSigner(),
	}
}

// RoundTrip implements http.RoundTripper.
func (transport *signingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	body, err := drainBody(req)

	if err != nil {
		return nil, fmt.Errorf("failed to read the request body: %w", err)
	}

	body = injectMetadata(body, transport.Metadata)

	// A RoundTripper must not modify the request it is given.
	signed := req.Clone(req.Context())
	setBody(signed, body)

	credentials, err := transport.Credentials.Retrieve(req.Context())

	if err != nil {
		return nil, fmt.Errorf("failed to retrieve AWS credentials: %w", err)
	}

	sum := sha256.Sum256(body)
	err = transport.signer.SignHTTP(
		req.Context(),
		credentials,
		signed,
		hex.EncodeToString(sum[:]),
		transport.Service,
		transport.Region,
		time.Now().UTC(),
	)

	if err != nil {
		return nil, fmt.Errorf("failed to sign the request: %w", err)
	}

	base := transport.Base

	if base == nil {
		base = http.DefaultTransport
	}

	return base.RoundTrip(signed)
}

// drainBody reads and closes the request body, returning nil for a bodyless
// request such as the GET that opens the server-to-client SSE stream.
func drainBody(req *http.Request) ([]byte, error) {
	if req.Body == nil || req.Body == http.NoBody {
		return nil, nil
	}

	defer func() { _ = req.Body.Close() }()

	return io.ReadAll(req.Body)
}

// setBody replaces the request body, keeping ContentLength and GetBody
// consistent so the signature stays valid if the request is replayed.
func setBody(req *http.Request, body []byte) {
	if len(body) == 0 {
		req.Body = http.NoBody
		req.ContentLength = 0
		req.GetBody = func() (io.ReadCloser, error) { return http.NoBody, nil }

		return
	}

	req.Body = io.NopCloser(bytes.NewReader(body))
	req.ContentLength = int64(len(body))
	req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(body)), nil }
}

// injectMetadata merges metadata into the "_meta" object of a JSON-RPC
// message's params. Values already present win, so an explicit value from the
// client is never overwritten.
//
// Anything that is not a JSON-RPC message with an object "params" -- including
// a bodyless request -- is returned unchanged.
func injectMetadata(body []byte, metadata map[string]string) []byte {
	if len(body) == 0 || len(metadata) == 0 {
		return body
	}

	var message map[string]any

	if err := json.Unmarshal(body, &message); err != nil {
		return body
	}

	if _, ok := message["jsonrpc"]; !ok {
		return body
	}

	params, ok := message["params"].(map[string]any)

	if !ok {
		return body
	}

	meta, ok := params[metaField].(map[string]any)

	if !ok {
		meta = map[string]any{}
	}

	for key, value := range metadata {
		if _, ok := meta[key]; !ok {
			meta[key] = value
		}
	}

	params[metaField] = meta

	injected, err := json.Marshal(message)

	if err != nil {
		return body
	}

	return injected
}
