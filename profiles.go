package awsmcproxy

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
)

// Section prefixes in the shared AWS config, mirroring aws-sdk-go-v2's
// config/shared_config.go.
const (
	profilePrefix    = "profile "
	ssoSessionPrefix = "sso-session "
	servicesPrefix   = "services "
	defaultProfile   = "default"
)

// Profiles returns the AWS profile names the SDK can resolve, in the order
// they appear: the shared config file first, then the shared credentials file.
//
// The naming rules follow aws-sdk-go-v2 rather than botocore, because these
// names are handed straight back to the SDK. In the config file a non-default
// profile must carry the "profile " prefix, which is stripped, while
// "sso-session"/"services" sections and any other unprefixed section are not
// profiles. In the credentials file the opposite holds: every section is a
// profile except one carrying the "profile " prefix.
func Profiles() ([]string, error) {
	var names []string

	seen := map[string]bool{}

	add := func(name string) {
		if name == "" || seen[name] {
			return
		}

		seen[name] = true
		names = append(names, name)
	}

	for _, path := range sharedFiles(awsSharedConfigFileEnv, awsconfig.DefaultSharedConfigFiles) {
		sections, err := readSections(path)

		if err != nil {
			return nil, err
		}

		for _, section := range sections {
			switch {
			case strings.HasPrefix(section, profilePrefix):
				add(strings.TrimPrefix(section, profilePrefix))
			case strings.HasPrefix(section, ssoSessionPrefix), strings.HasPrefix(section, servicesPrefix):
				// Not a profile.
			case strings.EqualFold(section, defaultProfile):
				add(section)
			}
			// Any other section is not a valid profile name and is ignored,
			// exactly as the SDK ignores it.
		}
	}

	for _, path := range sharedFiles(awsSharedCredentialsFileEnv, awsconfig.DefaultSharedCredentialsFiles) {
		sections, err := readSections(path)

		if err != nil {
			return nil, err
		}

		for _, section := range sections {
			// A "profile " prefix is invalid in the credentials file.
			if !strings.HasPrefix(section, profilePrefix) {
				add(section)
			}
		}
	}

	return names, nil
}

const (
	awsSharedConfigFileEnv      = "AWS_CONFIG_FILE"
	awsSharedCredentialsFileEnv = "AWS_SHARED_CREDENTIALS_FILE"
)

// sharedFiles resolves the files to read for one half of the shared
// configuration. The environment variable replaces the defaults outright, as
// it does in the SDK.
func sharedFiles(env string, defaults []string) []string {
	if path := os.Getenv(env); path != "" {
		return []string{path}
	}

	return defaults
}

// readSections returns the section names of an INI file in file order. A file
// that does not exist yields no sections, since either shared file is
// optional.
func readSections(path string) ([]string, error) {
	file, err := os.Open(path)

	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}

		return nil, fmt.Errorf("failed to read '%s': %w", path, err)
	}

	defer func() { _ = file.Close() }()

	var sections []string

	scanner := bufio.NewScanner(file)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())

		if !strings.HasPrefix(line, "[") {
			continue
		}

		end := strings.Index(line, "]")

		if end < 0 {
			continue
		}

		sections = append(sections, strings.TrimSpace(line[1:end]))
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("failed to read '%s': %w", path, err)
	}

	return sections, nil
}
