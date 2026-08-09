package awsmcproxy

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// recordingTransport captures the request that reached the bottom of the
// RoundTripper chain, after metadata injection and signing.
type recordingTransport struct {
	req  *http.Request
	body []byte
}

func (transport *recordingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	transport.req = req

	if req.Body != nil {
		body, err := io.ReadAll(req.Body)

		if err != nil {
			return nil, err
		}

		transport.body = body
	}

	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader("")),
		Header:     http.Header{},
		Request:    req,
	}, nil
}

func staticCredentials() aws.CredentialsProvider {
	return credentials.NewStaticCredentialsProvider("AKIAEXAMPLE", "secret", "token")
}

func newTestRequest(t *testing.T, body string) *http.Request {
	t.Helper()

	var reader io.Reader

	if body != "" {
		reader = strings.NewReader(body)
	}

	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, "https://aws-mcp.us-east-1.api.aws/mcp", reader)
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")

	return req
}

func TestSigningTransportSignsRequest(t *testing.T) {
	recorder := &recordingTransport{}
	transport := newSigningTransport(staticCredentials(), "aws-mcp", "us-east-1", nil)
	transport.Base = recorder

	req := newTestRequest(t, `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`)
	_, err := transport.RoundTrip(req)
	require.NoError(t, err)

	authorization := recorder.req.Header.Get("Authorization")
	assert.Contains(t, authorization, "AWS4-HMAC-SHA256")
	assert.Contains(t, authorization, "Credential=AKIAEXAMPLE/")
	assert.Contains(t, authorization, "/us-east-1/aws-mcp/aws4_request")
	assert.NotEmpty(t, recorder.req.Header.Get("X-Amz-Date"))
	assert.Equal(t, "token", recorder.req.Header.Get("X-Amz-Security-Token"))

	// The original request must not be modified.
	assert.Empty(t, req.Header.Get("Authorization"))
}

func TestSigningTransportInjectsMetadata(t *testing.T) {
	recorder := &recordingTransport{}
	transport := newSigningTransport(staticCredentials(), "aws-mcp", "us-east-1", map[string]string{"AWS_REGION": "ap-northeast-1"})
	transport.Base = recorder

	_, err := transport.RoundTrip(newTestRequest(t, `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`))
	require.NoError(t, err)

	var message struct {
		Params struct {
			Meta map[string]string `json:"_meta"`
		} `json:"params"`
	}
	require.NoError(t, json.Unmarshal(recorder.body, &message))

	assert.Equal(t, map[string]string{"AWS_REGION": "ap-northeast-1"}, message.Params.Meta)

	// The rewritten body has to be described accurately, or the signature will
	// not match the bytes on the wire.
	assert.Equal(t, int64(len(recorder.body)), recorder.req.ContentLength)

	replay, err := recorder.req.GetBody()
	require.NoError(t, err)
	replayed, err := io.ReadAll(replay)
	require.NoError(t, err)
	assert.Equal(t, recorder.body, replayed)
}

func TestSigningTransportBodylessRequest(t *testing.T) {
	recorder := &recordingTransport{}
	transport := newSigningTransport(staticCredentials(), "aws-mcp", "us-east-1", map[string]string{"AWS_REGION": "ap-northeast-1"})
	transport.Base = recorder

	// The standalone SSE stream is opened with a bodyless GET.
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://aws-mcp.us-east-1.api.aws/mcp", nil)
	require.NoError(t, err)

	_, err = transport.RoundTrip(req)
	require.NoError(t, err)

	assert.Empty(t, recorder.body)
	assert.Equal(t, int64(0), recorder.req.ContentLength)
	assert.Contains(t, recorder.req.Header.Get("Authorization"), "AWS4-HMAC-SHA256")
}

func TestSigningTransportCredentialsError(t *testing.T) {
	transport := newSigningTransport(
		aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
			return aws.Credentials{}, errors.New("expired")
		}),
		"aws-mcp", "us-east-1", nil,
	)
	transport.Base = &recordingTransport{}

	_, err := transport.RoundTrip(newTestRequest(t, `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`))
	require.Error(t, err)

	assert.Contains(t, err.Error(), "failed to retrieve AWS credentials")
}

// TestSigningTransportDefaultBase covers the http.DefaultTransport fallback.
func TestSigningTransportDefaultBase(t *testing.T) {
	var authorization string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		authorization = req.Header.Get("Authorization")
	}))
	defer server.Close()

	transport := newSigningTransport(staticCredentials(), "aws-mcp", "us-east-1", nil)

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, server.URL, nil)
	require.NoError(t, err)

	resp, err := transport.RoundTrip(req)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())

	assert.Contains(t, authorization, "AWS4-HMAC-SHA256")
}

func TestInjectMetadata(t *testing.T) {
	metadata := map[string]string{"AWS_REGION": "us-east-1"}

	tests := []struct {
		name string
		body string
		want string
	}{
		{
			name: "adds _meta",
			body: `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`,
			want: `{"id":1,"jsonrpc":"2.0","method":"tools/list","params":{"_meta":{"AWS_REGION":"us-east-1"}}}`,
		},
		{
			name: "keeps existing _meta entries",
			body: `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"_meta":{"AWS_REGION":"eu-west-1"}}}`,
			want: `{"id":1,"jsonrpc":"2.0","method":"tools/list","params":{"_meta":{"AWS_REGION":"eu-west-1"}}}`,
		},
		{
			name: "merges into existing _meta",
			body: `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"_meta":{"OTHER":"1"}}}`,
			want: `{"id":1,"jsonrpc":"2.0","method":"tools/list","params":{"_meta":{"AWS_REGION":"us-east-1","OTHER":"1"}}}`,
		},
		{
			name: "replaces a non-object _meta",
			body: `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"_meta":"nope"}}`,
			want: `{"id":1,"jsonrpc":"2.0","method":"tools/list","params":{"_meta":{"AWS_REGION":"us-east-1"}}}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.JSONEq(t, tt.want, string(injectMetadata([]byte(tt.body), metadata)))
		})
	}
}

// TestInjectMetadataPassthrough covers the bodies that are forwarded byte for
// byte because there is nowhere to put the metadata.
func TestInjectMetadataPassthrough(t *testing.T) {
	metadata := map[string]string{"AWS_REGION": "us-east-1"}

	tests := []struct {
		name string
		body string
	}{
		{"a message without params", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`},
		{"a message whose params is not an object", `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":[]}`},
		{"a non-JSON-RPC body", `{"params":{}}`},
		{"a JSON-RPC batch", `[{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}]`},
		{"a non-JSON body", `not json`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.body, string(injectMetadata([]byte(tt.body), metadata)))
		})
	}
}

func TestInjectMetadataNoop(t *testing.T) {
	body := []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`)

	assert.Equal(t, body, injectMetadata(body, nil))
	assert.Nil(t, injectMetadata(nil, map[string]string{"AWS_REGION": "us-east-1"}))
}
