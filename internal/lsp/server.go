package lsp

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"path/filepath"
	"strings"
	"sync"

	"github.com/hakastein/phplslow/internal/index"
	"github.com/hakastein/phplslow/internal/php"
)

// Serve speaks LSP on in and out until the client sends `exit` or closes in, and returns the process exit code:
// 0 after `shutdown`, 1 otherwise. A failed write to out ends the session with 1. Diagnostic lines go to log; they
// are not a protocol.
func Serve(in io.Reader, out io.Writer, log io.Writer) int {
	s := &server{out: newWriter(out), ready: make(chan struct{}), waiting: map[string]chan struct{}{}}
	var logMu sync.Mutex
	s.logf = func(format string, args ...any) {
		logMu.Lock()
		defer logMu.Unlock()
		fmt.Fprintf(log, format+"\n", args...)
	}
	messages, readErr := make(chan *message), make(chan error, 1)
	go func() {
		r := bufio.NewReader(in)
		for {
			m, err := read(r)
			if err != nil {
				readErr <- err
				return
			}
			messages <- m
		}
	}()
	defer s.close()
	for {
		select {
		case <-s.out.failed:
			s.logf("phplslow: write: %v", s.out.cause())
			return 1
		case err := <-readErr:
			if !errors.Is(err, io.EOF) {
				s.logf("phplslow: read: %v", err)
			}
			return s.exitCode()
		case m := <-messages:
			switch {
			case m.bad != nil:
				s.out.fail(nil, codeParseError, m.bad.Error())
			case m.ID != nil:
				s.request(m)
			case m.Method == "exit":
				return s.exitCode()
			case m.Method == "$/cancelRequest":
				s.cancel(m.Params)
			}
			// other notifications change nothing: answers come from the files on disk
		}
	}
}

type server struct {
	out  *writer
	logf func(format string, args ...any)

	mu          sync.Mutex
	initialized bool
	shutdown    bool
	root        string
	waiting     map[string]chan struct{} // requests waiting for the index by id; closing the channel cancels one

	ready chan struct{} // closed when ix or ixErr is set
	ix    *index.Index
	ixErr error
}

// close closes the index if it is open. An index still being built is left to the end of the process: its
// unfinished transaction leaves the previous state.
func (s *server) close() {
	select {
	case <-s.ready:
		if s.ix != nil {
			if err := s.ix.Close(); err != nil {
				s.logf("phplslow: close index: %v", err)
			}
		}
	default:
	}
}

func (s *server) exitCode() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.shutdown {
		return 0
	}
	return 1
}

func (s *server) request(m *message) {
	s.mu.Lock()
	initialized, shutdown := s.initialized, s.shutdown
	s.mu.Unlock()

	switch {
	case m.Method == "initialize":
		if initialized {
			s.out.fail(m.ID, codeInvalidRequest, "initialize: already initialized")
			return
		}
		s.initialize(m)
	case !initialized:
		s.out.fail(m.ID, codeServerNotInitialized, m.Method+": not initialized")
	case shutdown:
		s.out.fail(m.ID, codeInvalidRequest, m.Method+": after shutdown")
	case m.Method == "shutdown":
		s.mu.Lock()
		s.shutdown = true
		s.mu.Unlock()
		s.out.reply(m.ID, nil)
	case m.Method == "textDocument/definition":
		go s.definition(m, s.wait(m.ID))
	case m.Method == "textDocument/references":
		go s.references(m, s.wait(m.ID))
	default:
		s.out.fail(m.ID, codeMethodNotFound, m.Method+": not supported")
	}
}

type initializeParams struct {
	RootURI          *string `json:"rootUri"`
	RootPath         *string `json:"rootPath"`
	WorkspaceFolders []struct {
		URI string `json:"uri"`
	} `json:"workspaceFolders"`
}

// initialize answers with the capabilities and starts indexing the Workspace root in the background.
func (s *server) initialize(m *message) {
	var p initializeParams
	if err := json.Unmarshal(m.Params, &p); err != nil {
		s.out.fail(m.ID, codeInvalidParams, "initialize: "+err.Error())
		return
	}
	root := ""
	switch {
	case p.RootURI != nil:
		root = uriPath(*p.RootURI)
	case len(p.WorkspaceFolders) > 0:
		root = uriPath(p.WorkspaceFolders[0].URI)
	case p.RootPath != nil:
		root = *p.RootPath
	}
	if root == "" || !filepath.IsAbs(root) {
		s.out.fail(m.ID, codeInvalidParams, "initialize: the Workspace root must be an absolute file URI")
		return
	}
	root = filepath.Clean(root)

	s.mu.Lock()
	s.initialized = true
	s.root = root
	s.mu.Unlock()

	s.out.reply(m.ID, map[string]any{
		"capabilities": map[string]any{
			"definitionProvider": true,
			"referencesProvider": true,
		},
		"serverInfo": map[string]any{"name": "phplslow"},
	})

	go func() {
		s.ix, s.ixErr = index.Open(root, s.logf)
		if s.ixErr != nil {
			s.logf("phplslow: index: %v", s.ixErr)
		}
		close(s.ready)
	}()
}

type positionParams struct {
	TextDocument struct {
		URI string `json:"uri"`
	} `json:"textDocument"`
	Position struct {
		Line      int `json:"line"`
		Character int `json:"character"`
	} `json:"position"`
	Context struct {
		IncludeDeclaration bool `json:"includeDeclaration"`
	} `json:"context"`
}

type location struct {
	URI   string   `json:"uri"`
	Range lspRange `json:"range"`
}

type lspRange struct {
	Start lspPosition `json:"start"`
	End   lspPosition `json:"end"`
}

type lspPosition struct {
	Line      int `json:"line"`
	Character int `json:"character"`
}

// wait registers request id as waiting for the index; closing the returned channel cancels it.
func (s *server) wait(id json.RawMessage) chan struct{} {
	cancel := make(chan struct{})
	s.mu.Lock()
	defer s.mu.Unlock()
	s.waiting[idKey(id)] = cancel
	return cancel
}

// cancel handles `$/cancelRequest`: a request that waits for the index is answered RequestCancelled; an unknown or
// answered id changes nothing.
func (s *server) cancel(params json.RawMessage) {
	var p struct {
		ID json.RawMessage `json:"id"`
	}
	if json.Unmarshal(params, &p) != nil || p.ID == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key := idKey(p.ID)
	if cancel, ok := s.waiting[key]; ok {
		delete(s.waiting, key)
		close(cancel)
	}
}

// idKey is the request id as a map key: its compact JSON.
func idKey(id json.RawMessage) string {
	var b bytes.Buffer
	if json.Compact(&b, id) != nil {
		return string(id)
	}
	return b.String()
}

func (s *server) definition(m *message, cancel chan struct{}) {
	s.answer(m, cancel, func(ix *index.Index, file string, p positionParams) ([]index.Location, error) {
		return ix.Definition(file, php.Position{Line: p.Position.Line, Character: p.Position.Character})
	})
}

func (s *server) references(m *message, cancel chan struct{}) {
	s.answer(m, cancel, func(ix *index.Index, file string, p positionParams) ([]index.Location, error) {
		pos := php.Position{Line: p.Position.Line, Character: p.Position.Character}
		return ix.References(file, pos, p.Context.IncludeDeclaration)
	})
}

// answer waits until the index is ready and answers a position request from it: an answer is never given from
// an index that is not there. A request cancelled while it waits is answered RequestCancelled.
func (s *server) answer(m *message, cancel chan struct{},
	query func(*index.Index, string, positionParams) ([]index.Location, error)) {
	if !s.awaitIndex(m.ID, cancel) {
		s.out.fail(m.ID, codeRequestCancelled, m.Method+": cancelled")
		return
	}
	var p positionParams
	if err := json.Unmarshal(m.Params, &p); err != nil {
		s.out.fail(m.ID, codeInvalidParams, m.Method+": "+err.Error())
		return
	}
	if s.ixErr != nil {
		s.out.fail(m.ID, codeRequestFailed, m.Method+": index: "+s.ixErr.Error())
		return
	}
	s.mu.Lock()
	root := s.root
	s.mu.Unlock()

	out := []location{}
	file, ok := relative(root, uriPath(p.TextDocument.URI))
	if ok {
		found, err := query(s.ix, file, p)
		if err != nil {
			s.out.fail(m.ID, codeRequestFailed, m.Method+": "+err.Error())
			return
		}
		for _, l := range found {
			out = append(out, location{URI: pathURI(filepath.Join(root, filepath.FromSlash(l.Path))), Range: lspRange{
				Start: lspPosition{Line: l.Range.Start.Line, Character: l.Range.Start.Character},
				End:   lspPosition{Line: l.Range.End.Line, Character: l.Range.End.Character},
			}})
		}
	}
	s.out.reply(m.ID, out)
}

// awaitIndex waits until the index is ready or the request is cancelled, and reports whether the index is ready. The
// request stops waiting either way: a later cancel finds nothing.
func (s *server) awaitIndex(id json.RawMessage, cancel chan struct{}) bool {
	defer func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if key := idKey(id); s.waiting[key] == cancel {
			delete(s.waiting, key)
		}
	}()
	select {
	case <-s.ready:
		return true
	case <-cancel:
		return false
	}
}

// uriPath is the file path of a file URI; "" for any other URI.
func uriPath(uri string) string {
	u, err := url.Parse(uri)
	if err != nil || u.Scheme != "file" {
		return ""
	}
	return filepath.FromSlash(u.Path)
}

func pathURI(path string) string {
	return (&url.URL{Scheme: "file", Path: filepath.ToSlash(path)}).String()
}

// relative is path relative to root with forward slashes; false when path is outside root.
func relative(root, path string) (string, bool) {
	if path == "" {
		return "", false
	}
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	return filepath.ToSlash(rel), true
}
