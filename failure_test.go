package main_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The user's cache directory is a regular file: no index can be opened under it.
func TestIndexFailureAnswersRequestFailed(t *testing.T) {
	root, home := workspace(t, "methods"), filepath.Join(t.TempDir(), "home")
	require.NoError(t, os.WriteFile(home, nil, 0o600))
	c := start(t, home)
	c.initialize(root)

	m := c.await(c.send("textDocument/references", referencesParams(t, root, cartAdd, false)))

	require.NotNil(t, m.Error)
	assert.Equal(t, -32803, m.Error.Code)
	c.stop()
}
