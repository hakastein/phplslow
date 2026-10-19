//go:build unix

package main_test

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The Workspace holds a named pipe with a .php name: indexing cannot finish before the test writes the file's
// text into the pipe, so the request below is sent while the index is not ready.
func TestRequestBeforeReadyWaitsForTheIndex(t *testing.T) {
	root := workspace(t, "methods")
	gate := filepath.Join(root, "src", "gate.php")
	require.NoError(t, syscall.Mkfifo(gate, 0o600))
	c := start(t, cache(t))
	c.initialize(root)

	params := place{"src/app.php", "add('kiwi')"}.params(t, root)
	params["context"] = map[string]any{"includeDeclaration": false}
	id := c.send("textDocument/references", params)
	require.True(t, c.silent(500*time.Millisecond), "answered before the index was ready")

	text := "<?php\n\n$gate = new \\Shop\\Cart();\n$gate->add('gate');\n"
	pipe, err := os.OpenFile(gate, os.O_WRONLY, 0)
	require.NoError(t, err)
	_, err = pipe.WriteString(text)
	require.NoError(t, err)
	require.NoError(t, pipe.Close())

	m := c.await(id)
	require.Nil(t, m.Error)
	// The expected places are read from the files; the pipe becomes a plain file with the same text.
	require.NoError(t, os.Remove(gate))
	require.NoError(t, os.WriteFile(gate, []byte(text), 0o600))
	want := append([]place{{"src/gate.php", "add('gate')"}}, cartAddCalls...)
	assert.ElementsMatch(t, locations(t, root, want), decode(t, m.Result))
	c.stop()
}
