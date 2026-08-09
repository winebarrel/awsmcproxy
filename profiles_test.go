package awsmcproxy

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writeSharedFile writes an AWS shared config or credentials file and points
// the matching environment variable at it.
func writeSharedFile(t *testing.T, env string, name string, content string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), name)
	require.NoError(t, os.WriteFile(path, []byte(content), 0600))
	t.Setenv(env, path)

	return path
}

// useEmptySharedFiles points both shared files at paths that do not exist, so a
// test only sees what it writes itself.
func useEmptySharedFiles(t *testing.T) {
	t.Helper()

	dir := t.TempDir()
	t.Setenv(awsSharedConfigFileEnv, filepath.Join(dir, "config"))
	t.Setenv(awsSharedCredentialsFileEnv, filepath.Join(dir, "credentials"))
}

func TestProfilesFromConfigFile(t *testing.T) {
	useEmptySharedFiles(t)
	writeSharedFile(t, awsSharedConfigFileEnv, "config", `
[default]
region = us-east-1

[profile dev]
region = us-west-2

[profile prod]
region = ap-northeast-1

; A non-default profile must carry the "profile " prefix, so this one is not a
; profile at all.
[stray]
region = eu-west-1

[sso-session my-sso]
sso_region = us-east-1

[services my-services]
s3 =
  endpoint_url = http://localhost:4566
`)

	profiles, err := Profiles()
	require.NoError(t, err)

	assert.Equal(t, []string{"default", "dev", "prod"}, profiles)
}

func TestProfilesFromCredentialsFile(t *testing.T) {
	useEmptySharedFiles(t)
	writeSharedFile(t, awsSharedCredentialsFileEnv, "credentials", `
[default]
aws_access_key_id = AKIADEFAULT

[dev]
aws_access_key_id = AKIADEV

# The "profile " prefix is invalid in the credentials file.
[profile prod]
aws_access_key_id = AKIAPROD
`)

	profiles, err := Profiles()
	require.NoError(t, err)

	assert.Equal(t, []string{"default", "dev"}, profiles)
}

func TestProfilesMergesBothFiles(t *testing.T) {
	useEmptySharedFiles(t)
	writeSharedFile(t, awsSharedConfigFileEnv, "config", "[profile dev]\nregion = us-west-2\n")
	writeSharedFile(t, awsSharedCredentialsFileEnv, "credentials", "[dev]\naws_access_key_id = AKIADEV\n\n[creds-only]\naws_access_key_id = AKIAONLY\n")

	profiles, err := Profiles()
	require.NoError(t, err)

	// The config file comes first, and a profile in both files appears once.
	assert.Equal(t, []string{"dev", "creds-only"}, profiles)
}

func TestProfilesCaseInsensitiveDefault(t *testing.T) {
	useEmptySharedFiles(t)
	// The SDK matches the default section with EqualFold, unlike botocore.
	writeSharedFile(t, awsSharedConfigFileEnv, "config", "[Default]\nregion = us-east-1\n")

	profiles, err := Profiles()
	require.NoError(t, err)

	assert.Equal(t, []string{"Default"}, profiles)
}

func TestProfilesMissingFiles(t *testing.T) {
	useEmptySharedFiles(t)

	profiles, err := Profiles()
	require.NoError(t, err)

	assert.Empty(t, profiles)
}

func TestProfilesUnreadableFile(t *testing.T) {
	useEmptySharedFiles(t)
	// A directory where a file is expected fails to read as one.
	t.Setenv(awsSharedConfigFileEnv, t.TempDir())

	_, err := Profiles()
	require.Error(t, err)

	assert.Contains(t, err.Error(), "failed to read")
}

func TestReadSections(t *testing.T) {
	path := writeSharedFile(t, awsSharedConfigFileEnv, "config", `
# comment
[ profile dev ]
region = us-west-2
key = [not-a-section]
[unterminated
[prod]
`)

	sections, err := readSections(path)
	require.NoError(t, err)

	assert.Equal(t, []string{"profile dev", "prod"}, sections)
}

func TestSharedFiles(t *testing.T) {
	t.Setenv("TEST_SHARED_FILE", "")
	assert.Equal(t, []string{"a", "b"}, sharedFiles("TEST_SHARED_FILE", []string{"a", "b"}))

	// The environment variable replaces the defaults outright.
	t.Setenv("TEST_SHARED_FILE", "/tmp/custom")
	assert.Equal(t, []string{"/tmp/custom"}, sharedFiles("TEST_SHARED_FILE", []string{"a", "b"}))
}
