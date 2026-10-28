package main_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// upstreamParserBinary builds phplslow from this source with the php-parser release instead of the fork go.mod
// replaces it with: the same indexer with another parser. The build needs the module proxy.
func upstreamParserBinary(t *testing.T) string {
	t.Helper()
	src := t.TempDir()
	for _, name := range []string{"go.mod", "go.sum", "main.go"} {
		data, err := os.ReadFile(name)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(src, name), data, 0o600))
	}
	require.NoError(t, os.CopyFS(filepath.Join(src, "internal"), os.DirFS("internal")))
	bin := filepath.Join(t.TempDir(), "phplslow")
	for _, args := range [][]string{
		{"mod", "edit", "-dropreplace=github.com/VKCOM/php-parser"},
		{"build", "-mod=mod", "-o", bin, "."},
	} {
		cmd := exec.Command("go", args...)
		cmd.Dir = src
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "go %v:\n%s", args, out)
	}
	return bin
}

// An index built with another parser holds the facts that parser extracted; the snapshot of files alone cannot tell.
func TestIndexOfAnotherParserIsNotReused(t *testing.T) {
	root, home := workspace(t, "syntax"), cache(t)

	upstream := startBinary(t, upstreamParserBinary(t), home)
	upstream.initialize(root)
	upstreamReferences := references(upstream, root, enumLabel, false)
	upstream.stop()

	fork := start(t, home)
	fork.initialize(root)
	forkReferences := references(fork, root, enumLabel, false)
	fork.stop()

	assert.Empty(t, upstreamReferences, "the upstream parser loses the class named Enum")
	assert.ElementsMatch(t, locations(t, root, []place{enumCall}), forkReferences)
	assert.Contains(t, fork.stderr.String(), built)
	assert.NotContains(t, fork.stderr.String(), reused)
}
