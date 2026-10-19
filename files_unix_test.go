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

// unreadable is a Workspace entry that holds a call to Shop\Cart::add and that the test makes unreadable and
// readable again.
type unreadable struct {
	name        string
	permissions bool // made unreadable by its mode bits, which do not stop a process running as root
	arrange     func(t *testing.T, root string) (spoil, mend func())
	path        string // the entry the index cannot read, relative to the root
	call        place
	osError     string
}

var unreadableEntries = []unreadable{
	{"unreadable .php file", true, unreadableFile, "src/file.php", place{"src/file.php", "add('linked')"},
		"permission denied"},
	{"unreadable directory", true, unreadableDirectory, "src/lib", place{"src/lib/deep.php", "add('linked')"},
		"permission denied"},
	{"unreadable symlinked directory", true, unreadableLinkedDirectory, "src/lib",
		place{"src/lib/deep.php", "add('linked')"}, "permission denied"},
	{"broken .php symlink", false, brokenPHPLink, "src/broken.php", place{"src/broken.php", "add('linked')"},
		"no such file or directory"},
}

// An entry the index cannot read fails the index: an answer without its calls would look complete.
func TestUnreadableEntryFailsTheIndex(t *testing.T) {
	for _, tt := range unreadableEntries {
		t.Run(tt.name, func(t *testing.T) {
			skipAsRoot(t, tt.permissions)
			root, home := workspace(t, "methods"), cache(t)
			spoil, mend := tt.arrange(t, root)
			params, callParams := referencesParams(t, root, cartAdd, false), tt.call.params(t, root)
			spoil()

			failing := start(t, home)
			failing.initialize(root)
			waiting := failing.await(failing.send("textDocument/references", params))
			later := failing.await(failing.send("textDocument/definition", callParams))
			failing.stop()
			mend()
			mended := start(t, home)
			mended.initialize(root)
			got := references(mended, root, cartAdd, false)
			mended.stop()

			assertIndexFailed(t, waiting, root, tt.path, tt.osError)
			assertIndexFailed(t, later, root, tt.path, tt.osError)
			assert.NotContains(t, failing.stderr.String(), built)
			assert.ElementsMatch(t, locations(t, root, append([]place{tt.call}, cartAddCalls...)), got)
			assert.Contains(t, mended.stderr.String(), built+"5 files")
		})
	}
}

// A ready index is not reused when an entry cannot be read: without the entry the snapshot cannot be compared.
// The ready index is built before the entry exists, so skipping the entry would make the snapshot match.
func TestUnreadableEntryFailsAReadyIndex(t *testing.T) {
	for _, tt := range unreadableEntries {
		t.Run(tt.name, func(t *testing.T) {
			skipAsRoot(t, tt.permissions)
			root, home := workspace(t, "methods"), cache(t)
			params := referencesParams(t, root, cartAdd, false)

			first := start(t, home)
			first.initialize(root)
			before := references(first, root, cartAdd, false)
			first.stop()
			spoil, mend := tt.arrange(t, root)
			want := locations(t, root, append([]place{tt.call}, cartAddCalls...))
			spoil()
			failing := start(t, home)
			failing.initialize(root)
			waiting := failing.await(failing.send("textDocument/references", params))
			failing.stop()
			mend()
			mended := start(t, home)
			mended.initialize(root)
			after := references(mended, root, cartAdd, false)
			mended.stop()

			assert.ElementsMatch(t, locations(t, root, cartAddCalls), before)
			assertIndexFailed(t, waiting, root, tt.path, tt.osError)
			assert.NotContains(t, failing.stderr.String(), reused)
			assert.ElementsMatch(t, want, after)
		})
	}
}

// A broken symlink without the .php suffix may have pointed at a directory or at anything else; it is skipped.
func TestBrokenSymlinkWithAnotherNameIsSkipped(t *testing.T) {
	root := workspace(t, "methods")
	require.NoError(t, os.Symlink(filepath.Join(t.TempDir(), "gone"), filepath.Join(root, "src", "gone")))
	c := start(t, cache(t))
	c.initialize(root)

	assert.ElementsMatch(t, locations(t, root, cartAddCalls), references(c, root, cartAdd, false))
	c.stop()
	assert.Contains(t, c.stderr.String(), "phplslow: skip "+filepath.Join(root, "src", "gone"))
}

func skipAsRoot(t *testing.T, permissions bool) {
	t.Helper()
	if permissions && os.Geteuid() == 0 {
		t.Skip("running as root: mode bits do not stop root from reading, so the entry cannot be made unreadable")
	}
}

func assertIndexFailed(t *testing.T, m response, root, path, osError string) {
	t.Helper()
	require.NotNil(t, m.Error, "answered from an index that misses %s", path)
	assert.Equal(t, -32803, m.Error.Code)
	assert.Contains(t, m.Error.Message, filepath.Join(root, filepath.FromSlash(path))+": "+osError)
}

// chmod sets the mode of path and puts it back to restore when the test ends, so the test can remove its files.
func chmod(t *testing.T, path string, mode, restore os.FileMode) (spoil, mend func()) {
	t.Helper()
	mend = func() { require.NoError(t, os.Chmod(path, restore)) }
	t.Cleanup(mend)
	return func() { require.NoError(t, os.Chmod(path, mode)) }, mend
}

func unreadableFile(t *testing.T, root string) (spoil, mend func()) {
	t.Helper()
	file := filepath.Join(root, "src", "file.php")
	require.NoError(t, os.WriteFile(file, []byte(linkedCall), 0o600))
	return chmod(t, file, 0, 0o600)
}

func unreadableDirectory(t *testing.T, root string) (spoil, mend func()) {
	t.Helper()
	lib := filepath.Join(root, "src", "lib")
	require.NoError(t, os.Mkdir(lib, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(lib, "deep.php"), []byte(linkedCall), 0o600))
	return chmod(t, lib, 0, 0o755)
}

func unreadableLinkedDirectory(t *testing.T, root string) (spoil, mend func()) {
	t.Helper()
	lib := filepath.Join(t.TempDir(), "lib")
	require.NoError(t, os.Mkdir(lib, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(lib, "deep.php"), []byte(linkedCall), 0o600))
	require.NoError(t, os.Symlink(lib, filepath.Join(root, "src", "lib")))
	return chmod(t, lib, 0, 0o755)
}

func brokenPHPLink(t *testing.T, root string) (spoil, mend func()) {
	t.Helper()
	target := filepath.Join(t.TempDir(), "linked.php")
	write := func() { require.NoError(t, os.WriteFile(target, []byte(linkedCall), 0o600)) }
	write()
	require.NoError(t, os.Symlink(target, filepath.Join(root, "src", "broken.php")))
	return func() { require.NoError(t, os.Remove(target)) }, write
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
