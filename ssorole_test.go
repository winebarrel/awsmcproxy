package awsmcproxy

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const ssoStartURL = "https://example.awsapps.com/start"

// setupSSOProfile writes an IAM Identity Center profile together with the
// cached access token `aws sso login` would have left behind, and points the
// SSO client at fake, which stands in for the identity center portal.
//
// The pre-sso-session form is used because it reads the cached token directly,
// with no OIDC round trip.
func setupSSOProfile(t *testing.T, fake *httptest.Server) {
	t.Helper()

	useEmptySharedFiles(t)
	writeSharedFile(t, awsSharedConfigFileEnv, "config", fmt.Sprintf(`
[profile dev]
sso_account_id = 111111111111
sso_role_name = AdministratorAccess
sso_region = us-east-1
sso_start_url = %s
region = us-east-1
`, ssoStartURL))

	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	sum := sha1.Sum([]byte(ssoStartURL))
	cache := filepath.Join(home, ".aws", "sso", "cache")
	require.NoError(t, os.MkdirAll(cache, 0700))
	require.NoError(t, os.WriteFile(
		filepath.Join(cache, hex.EncodeToString(sum[:])+".json"),
		fmt.Appendf(nil, `{"accessToken":"sso-access-token","expiresAt":%q}`,
			time.Now().Add(time.Hour).UTC().Format(time.RFC3339)),
		0600,
	))

	t.Setenv("AWS_ENDPOINT_URL_SSO", fake.URL)
	t.Setenv("AWS_PROFILE", "")
	t.Setenv("AWS_ACCESS_KEY_ID", "")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "")
	t.Setenv("AWS_SESSION_TOKEN", "")
	t.Setenv("AWS_REGION", "")
	t.Setenv("AWS_DEFAULT_REGION", "")
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
}

// newFakeSSOPortal answers GetRoleCredentials and records what was asked for.
func newFakeSSOPortal(t *testing.T, asked *url.Values) *httptest.Server {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		*asked = req.URL.Query()

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"roleCredentials":{` +
			`"accessKeyId":"AKIASSO","secretAccessKey":"ssosecret",` +
			`"sessionToken":"ssotoken","expiration":99999999999999}}`))
	}))
	t.Cleanup(server.Close)

	return server
}

// retrieve pulls credentials through the proxy's signing transport, which is
// what actually signs a request.
func retrieve(t *testing.T, proxy *Proxy, profile string) aws.Credentials {
	t.Helper()

	client, err := proxy.httpClient(context.Background(), profile)
	require.NoError(t, err)

	transport, ok := client.Transport.(*signingTransport)
	require.True(t, ok)

	credentials, err := transport.Credentials.Retrieve(context.Background())
	require.NoError(t, err)

	return credentials
}

func TestSSORoleOverridesProfileRole(t *testing.T) {
	var asked url.Values

	setupSSOProfile(t, newFakeSSOPortal(t, &asked))

	proxy := testProxy(t, "https://aws-mcp.us-east-1.api.aws/mcp")
	proxy.ssoRole = "ReadOnlyAccess"

	credentials := retrieve(t, proxy, "dev")

	// The role name is the overridden one, while the account and the SSO
	// session come from the profile untouched.
	assert.Equal(t, "ReadOnlyAccess", asked.Get("role_name"))
	assert.Equal(t, "111111111111", asked.Get("account_id"))
	assert.Equal(t, "AKIASSO", credentials.AccessKeyID)
}

func TestWithoutSSORoleTheProfileRoleIsUsed(t *testing.T) {
	var asked url.Values

	setupSSOProfile(t, newFakeSSOPortal(t, &asked))

	retrieve(t, testProxy(t, "https://aws-mcp.us-east-1.api.aws/mcp"), "dev")

	assert.Equal(t, "AdministratorAccess", asked.Get("role_name"))
}

// TestSSORoleAppliesToTheDefaultChain covers the connection the proxy opens at
// startup, which names no profile.
func TestSSORoleAppliesToTheDefaultChain(t *testing.T) {
	var asked url.Values

	setupSSOProfile(t, newFakeSSOPortal(t, &asked))
	t.Setenv("AWS_PROFILE", "dev")

	proxy := testProxy(t, "https://aws-mcp.us-east-1.api.aws/mcp")
	proxy.ssoRole = "ReadOnlyAccess"

	retrieve(t, proxy, "")

	assert.Equal(t, "ReadOnlyAccess", asked.Get("role_name"))
}

// TestSSORoleIgnoredForNonSSOProfile checks that a profile that does not use
// IAM Identity Center keeps working: there is no sso_role_name to override, so
// the option is never consulted.
func TestSSORoleIgnoredForNonSSOProfile(t *testing.T) {
	setupAWSProfiles(t)

	proxy := testProxy(t, "https://aws-mcp.us-east-1.api.aws/mcp")
	proxy.ssoRole = "ReadOnlyAccess"

	credentials := retrieve(t, proxy, "dev")

	assert.Equal(t, "AKIADEV", credentials.AccessKeyID)
}

func TestNewProxyCarriesSSORole(t *testing.T) {
	proxy, err := NewProxy(&Options{SSORole: "ReadOnlyAccess"}, "test")
	require.NoError(t, err)

	assert.Equal(t, "ReadOnlyAccess", proxy.ssoRole)
}
