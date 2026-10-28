package main_test

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/textproto"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf16"

	"github.com/ncruces/go-sqlite3"
	"github.com/stretchr/testify/require"
)

// binary is phplslow built without CGO once for the whole test run; every test talks to it over stdio.
var binary string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "phplslow-test-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	binary = filepath.Join(dir, "phplslow")
	build := exec.Command("go", "build", "-o", binary, ".")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	build.Stdout, build.Stderr = os.Stderr, os.Stderr
	code := 1
	if err := build.Run(); err == nil {
		code = m.Run()
	}
	os.RemoveAll(dir)
	os.Exit(code)
}

// patience bounds every wait for the server.
const patience = 30 * time.Second

// workspace copies testdata/<name> into a fresh directory and returns its root.
func workspace(t *testing.T, name string) string {
	t.Helper()
	return workspaceAt(t, name, "ws")
}

// workspaceAt copies testdata/<name> into a fresh directory named dir and returns its root.
func workspaceAt(t *testing.T, name, dir string) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), dir)
	require.NoError(t, os.CopyFS(root, os.DirFS(filepath.Join("testdata", name))))
	return root
}

// indexFile is the index file of root that ADR 0001 names for a process whose user cache directory is home.
func indexFile(home, root string) string {
	dir := home
	if runtime.GOOS == "darwin" {
		dir = filepath.Join(home, "Library", "Caches")
	}
	sum := sha256.Sum256([]byte(root))
	return filepath.Join(dir, "phplslow", indexDir, hex.EncodeToString(sum[:16]), "index.sqlite")
}

// indexDir is the directory ADR 0001 names for the indexes of the test binary: the schema version and the parser
// module the binary is linked with.
var indexDir = func() string {
	out, err := exec.Command("go", "list", "-m", "-f",
		"{{with .Replace}}{{.Path}}@{{.Version}}{{else}}{{.Path}}@{{.Version}}{{end}}",
		"github.com/VKCOM/php-parser").Output()
	if err != nil {
		panic(fmt.Sprintf("go list the parser module: %v", err))
	}
	sum := sha256.Sum256(bytes.TrimSpace(out))
	return "index-v2-" + hex.EncodeToString(sum[:6])
}()

// lockIndex holds an exclusive lock on the index file of root for processes with home as their cache directory:
// such a process can neither read nor build that index until the returned func releases the lock.
func lockIndex(t *testing.T, home, root string) func() {
	t.Helper()
	path := indexFile(home, root)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	conn, err := sqlite3.Open(path)
	require.NoError(t, err)
	require.NoError(t, conn.Exec(`BEGIN EXCLUSIVE`))
	var once sync.Once
	release := func() {
		once.Do(func() {
			require.NoError(t, conn.Exec(`ROLLBACK`))
			require.NoError(t, conn.Close())
		})
	}
	t.Cleanup(release)
	return release
}

// cache is a directory that stands for the user's cache directory of one or more phplslow processes.
func cache(t *testing.T) string {
	t.Helper()
	return t.TempDir()
}

type response struct {
	ID     int             `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// client is one phplslow process and the LSP session with it.
type client struct {
	t         *testing.T
	cmd       *exec.Cmd
	stdin     io.WriteCloser
	responses chan response
	stderr    *syncBuffer
	exited    chan struct{} // closed when the process has exited; exitErr is set then
	exitErr   error
	next      int
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// start runs phplslow with home as the user's home and cache directory.
func start(t *testing.T, home string) *client {
	t.Helper()
	return startBinary(t, binary, home)
}

// startBinary runs the phplslow binary at path like start.
func startBinary(t *testing.T, path, home string) *client {
	t.Helper()
	cmd := exec.Command(path)
	cmd.Env = append(os.Environ(), "HOME="+home, "XDG_CACHE_HOME="+home, "LOCALAPPDATA="+home)
	stdin, err := cmd.StdinPipe()
	require.NoError(t, err)
	stdout, err := cmd.StdoutPipe()
	require.NoError(t, err)
	c := &client{t: t, cmd: cmd, stdin: stdin, responses: make(chan response, 16), stderr: &syncBuffer{},
		exited: make(chan struct{})}
	cmd.Stderr = c.stderr
	require.NoError(t, cmd.Start())
	t.Cleanup(func() {
		cmd.Process.Kill()
		<-c.exited
	})
	go func() {
		r := bufio.NewReader(stdout)
		for {
			header, err := textproto.NewReader(r).ReadMIMEHeader()
			if err != nil {
				break
			}
			n, _ := strconv.Atoi(header.Get("Content-Length"))
			body := make([]byte, n)
			if _, err := io.ReadFull(r, body); err != nil {
				break
			}
			var m response
			if json.Unmarshal(body, &m) == nil {
				c.responses <- m
			}
		}
		close(c.responses)
		c.exitErr = cmd.Wait()
		close(c.exited)
	}()
	return c
}

// startWithoutReader runs phplslow like start, but with a stdout whose reading end is already closed: every write
// of the process to stdout fails.
func startWithoutReader(t *testing.T, home string) *client {
	t.Helper()
	cmd := exec.Command(binary)
	cmd.Env = append(os.Environ(), "HOME="+home, "XDG_CACHE_HOME="+home, "LOCALAPPDATA="+home)
	stdin, err := cmd.StdinPipe()
	require.NoError(t, err)
	r, w, err := os.Pipe()
	require.NoError(t, err)
	require.NoError(t, r.Close())
	cmd.Stdout = w
	c := &client{t: t, cmd: cmd, stdin: stdin, responses: make(chan response), stderr: &syncBuffer{},
		exited: make(chan struct{})}
	cmd.Stderr = c.stderr
	require.NoError(t, cmd.Start())
	require.NoError(t, w.Close())
	t.Cleanup(func() {
		cmd.Process.Kill()
		<-c.exited
	})
	go func() {
		c.exitErr = cmd.Wait()
		close(c.responses)
		close(c.exited)
	}()
	return c
}

// exitCode waits for the process to exit and returns its exit code; -1 when a signal ended it.
func (c *client) exitCode() int {
	c.t.Helper()
	select {
	case <-c.exited:
		return c.cmd.ProcessState.ExitCode()
	case <-time.After(patience):
		require.FailNow(c.t, "phplslow did not exit", "stderr:\n%s", c.stderr)
		return 0
	}
}

func (c *client) write(m map[string]any) {
	c.t.Helper()
	m["jsonrpc"] = "2.0"
	body, err := json.Marshal(m)
	require.NoError(c.t, err)
	_, err = fmt.Fprintf(c.stdin, "Content-Length: %d\r\n\r\n%s", len(body), body)
	require.NoError(c.t, err)
}

// send sends a request and returns its id without waiting for the answer.
func (c *client) send(method string, params any) int {
	c.t.Helper()
	c.next++
	c.write(map[string]any{"id": c.next, "method": method, "params": params})
	return c.next
}

func (c *client) notify(method string, params any) {
	c.t.Helper()
	c.write(map[string]any{"method": method, "params": params})
}

// cancel asks the server to cancel request id.
func (c *client) cancel(id int) {
	c.t.Helper()
	c.notify("$/cancelRequest", map[string]any{"id": id})
}

// await is the response to request id. The server answers requests one at a time in these tests, so the next
// response is the one awaited.
func (c *client) await(id int) response {
	c.t.Helper()
	select {
	case m, ok := <-c.responses:
		require.True(c.t, ok, "phplslow closed stdout; stderr:\n%s", c.stderr)
		require.Equal(c.t, id, m.ID)
		return m
	case <-time.After(patience):
		require.FailNow(c.t, "no response", "request %d; stderr:\n%s", id, c.stderr)
		return response{}
	}
}

// silent reports whether no response arrives for d.
func (c *client) silent(d time.Duration) bool {
	select {
	case <-c.responses:
		return false
	case <-time.After(d):
		return true
	}
}

func (c *client) call(method string, params any) json.RawMessage {
	c.t.Helper()
	m := c.await(c.send(method, params))
	require.Nil(c.t, m.Error, "%s; stderr:\n%s", method, c.stderr)
	return m.Result
}

// initialize opens the session on the Workspace root.
func (c *client) initialize(root string) {
	c.t.Helper()
	c.call("initialize", map[string]any{"processId": nil, "rootUri": uri(root), "capabilities": map[string]any{}})
	c.notify("initialized", map[string]any{})
}

// stop ends the session the way a client does and checks that the process exits with 0.
func (c *client) stop() {
	c.t.Helper()
	c.call("shutdown", nil)
	c.notify("exit", nil)
	select {
	case <-c.exited:
		require.NoError(c.t, c.exitErr)
	case <-time.After(patience):
		require.FailNow(c.t, "phplslow did not exit")
	}
}

type position struct {
	Line      int `json:"line"`
	Character int `json:"character"`
}

type span struct {
	Start position `json:"start"`
	End   position `json:"end"`
}

type location struct {
	URI   string `json:"uri"`
	Range span   `json:"range"`
}

// place is where a needle stands in a file of the Workspace.
type place struct {
	file   string // relative to the Workspace root
	needle string // unique in the file; the place is the identifier it starts with
}

func uri(path string) string {
	return (&url.URL{Scheme: "file", Path: filepath.ToSlash(path)}).String()
}

// location is the LSP location of p's identifier.
func (p place) location(t *testing.T, root string) location {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(p.file))
	src, err := os.ReadFile(path)
	require.NoError(t, err)
	text := string(src)
	require.Equal(t, 1, strings.Count(text, p.needle), "needle %q in %s", p.needle, p.file)
	at := strings.Index(text, p.needle)
	word := len(p.needle) - len(strings.TrimLeft(p.needle,
		"abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_"))
	line := strings.Count(text[:at], "\n")
	column := len(utf16.Encode([]rune(text[strings.LastIndex(text[:at], "\n")+1 : at])))
	return location{URI: uri(path), Range: span{
		Start: position{Line: line, Character: column},
		End:   position{Line: line, Character: column + word},
	}}
}

func (p place) params(t *testing.T, root string) map[string]any {
	t.Helper()
	l := p.location(t, root)
	return map[string]any{"textDocument": map[string]any{"uri": l.URI}, "position": l.Range.Start}
}

func locations(t *testing.T, root string, places []place) []location {
	t.Helper()
	out := []location{}
	for _, p := range places {
		out = append(out, p.location(t, root))
	}
	return out
}

func decode(t *testing.T, raw json.RawMessage) []location {
	t.Helper()
	out := []location{}
	require.NoError(t, json.Unmarshal(raw, &out))
	return out
}

func definition(c *client, root string, at place) []location {
	c.t.Helper()
	return decode(c.t, c.call("textDocument/definition", at.params(c.t, root)))
}

func references(c *client, root string, at place, declaration bool) []location {
	c.t.Helper()
	return decode(c.t, c.call("textDocument/references", referencesParams(c.t, root, at, declaration)))
}

func referencesParams(t *testing.T, root string, at place, declaration bool) map[string]any {
	t.Helper()
	params := at.params(t, root)
	params["context"] = map[string]any{"includeDeclaration": declaration}
	return params
}
