//go:build unix

package main_test

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const linkedCall = "<?php\n\nfunction linked(): void\n{\n" +
	"    $linked = new \\Shop\\Cart();\n    $linked->add('linked');\n}\n"

// A named pipe and a symlink to one have .php names; reading either would wait for a writer forever.
func TestNonRegularFilesAreNotIndexed(t *testing.T) {
	root, outside := workspace(t, "methods"), t.TempDir()
	require.NoError(t, syscall.Mkfifo(filepath.Join(root, "src", "x.php"), 0o600))
	require.NoError(t, syscall.Mkfifo(filepath.Join(outside, "pipe.php"), 0o600))
	require.NoError(t, os.Symlink(filepath.Join(outside, "pipe.php"), filepath.Join(root, "src", "y.php")))
	c := start(t, cache(t))
	c.initialize(root)

	assert.ElementsMatch(t, locations(t, root, cartAddCalls), references(c, root, cartAdd, false))
	c.stop()
}

func TestSymlinksAreFollowed(t *testing.T) {
	root, outside := workspace(t, "methods"), t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(outside, "linked.php"), []byte(linkedCall), 0o600))
	require.NoError(t, os.Mkdir(filepath.Join(outside, "lib"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(outside, "lib", "deep.php"), []byte(linkedCall), 0o600))
	require.NoError(t, os.Symlink(filepath.Join(outside, "linked.php"), filepath.Join(root, "src", "file.php")))
	require.NoError(t, os.Symlink(filepath.Join(outside, "lib"), filepath.Join(root, "src", "lib")))
	c := start(t, cache(t))
	c.initialize(root)

	want := append([]place{{"src/file.php", "add('linked')"}, {"src/lib/deep.php", "add('linked')"}}, cartAddCalls...)
	assert.ElementsMatch(t, locations(t, root, want), references(c, root, cartAdd, false))
	c.stop()
}

// src/loop/ is the Workspace root again; walking into it would never end.
func TestSymlinkCycleIsWalkedOnce(t *testing.T) {
	root := workspace(t, "methods")
	require.NoError(t, os.Symlink(root, filepath.Join(root, "src", "loop")))
	c := start(t, cache(t))
	c.initialize(root)

	assert.ElementsMatch(t, locations(t, root, cartAddCalls), references(c, root, cartAdd, false))
	c.stop()
}

// Writing to stdout fails once the client has closed its end: the session ends instead of serving no one.
func TestFailedWriteEndsTheSession(t *testing.T) {
	root := workspace(t, "methods")
	c := startWithoutReader(t, cache(t))
	c.send("initialize", map[string]any{"processId": nil, "rootUri": uri(root), "capabilities": map[string]any{}})

	code := c.exitCode()
	assert.Equal(t, 1, code)
	assert.Contains(t, c.stderr.String(), "broken pipe")
}
