package awsmcproxy

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestInjectProfileArg(t *testing.T) {
	schema, err := injectProfileArg(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"cli_command": map[string]any{"type": "string"},
		},
		"required": []any{"cli_command"},
	}, []string{"dev", "prod"})
	require.NoError(t, err)

	properties, ok := schema["properties"].(map[string]any)
	require.True(t, ok)

	assert.Contains(t, properties, "cli_command")

	profile, ok := properties[profileArg].(map[string]any)
	require.True(t, ok)

	assert.Equal(t, "string", profile["type"])
	assert.Equal(t, []any{"dev", "prod"}, profile["enum"])
	assert.Contains(t, profile["description"], "dev, prod")

	// The injected argument leads, and the upstream requirements are kept.
	assert.Equal(t, []any{profileArg, "cli_command"}, schema["required"])
}

func TestInjectProfileArgDoesNotMutateInput(t *testing.T) {
	properties := map[string]any{}
	input := map[string]any{"type": "object", "properties": properties}

	_, err := injectProfileArg(input, []string{"dev"})
	require.NoError(t, err)

	assert.Empty(t, properties)
}

func TestInjectProfileArgEmptySchema(t *testing.T) {
	for _, input := range []any{nil, map[string]any{}} {
		schema, err := injectProfileArg(input, []string{"dev"})
		require.NoError(t, err)

		assert.Equal(t, "object", schema["type"])

		properties, ok := schema["properties"].(map[string]any)
		require.True(t, ok)

		assert.Contains(t, properties, profileArg)
		assert.Equal(t, []any{profileArg}, schema["required"])
	}
}

func TestInjectProfileArgUnmarshalableSchema(t *testing.T) {
	_, err := injectProfileArg(func() {}, []string{"dev"})
	require.Error(t, err)
}

func TestInjectProfileArgNonObjectSchema(t *testing.T) {
	// A schema the upstream server sent as something other than a JSON object
	// must not panic the proxy.
	_, err := injectProfileArg([]any{"nope"}, []string{"dev"})
	require.Error(t, err)
}

func TestPrependRequired(t *testing.T) {
	assert.Equal(t, []any{"profile"}, prependRequired(nil, "profile"))
	assert.Equal(t, []any{"profile", "a"}, prependRequired([]any{"a"}, "profile"))
	// No duplicate when the upstream schema already requires it.
	assert.Equal(t, []any{"profile", "a"}, prependRequired([]any{"profile", "a"}, "profile"))
	// A "required" of an unexpected type is replaced rather than merged.
	assert.Equal(t, []any{"profile"}, prependRequired("a", "profile"))
}

func TestErrorResult(t *testing.T) {
	result := errorResult("boom: %s", "detail")

	assert.True(t, result.IsError)
	assert.Equal(t, "boom: detail", resultText(result))
}
