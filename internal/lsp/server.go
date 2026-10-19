package lsp

import (
	"bufio"
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
// 0 after `shutdown`, 1 otherwise. Diagnostic lines go to log; they are not a protocol.
func Serve(in io.Reader, out io.Writer, log io.Writer) int {
	s := &server{out: &writer{w: out}, ready: make(chan struct{})}
	var logMu sync.Mutex
	s.logf = func(format string, args ...any) {
		logMu.Lock()
		defer logMu.Unlock()
		fmt.Fprintf(log, format+"\n", args...)
	}
	r := bufio.NewReader(in)
	for {
		m, err := read(r)
		if err != nil {
			if !errors.Is(err, io.EOF) {
				s.logf("phplslow: read: %v", err)
			}
			return s.exitCode()
		}
		if m.bad != nil {
			s.out.fail(nil, codeParseError, m.bad.Error())
			continue
		}
		if m.ID == nil {
			if m.Method == "exit" {
				return s.exitCode()
			}
			continue // other notifications change nothing: answers come from the files on disk
		}
		s.request(m)
	}
}

type server struct {
	out  *writer
	logf func(format string, args ...any)

	mu          sync.Mutex
	initialized bool
	shutdown    bool
	root        string

	ready chan struct{} // closed when ix or ixErr is set
	ix    *index.Index
	ixErr error
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
		go s.definition(m)
	case m.Method == "textDocument/references":
		go s.references(m)
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

func (s *server) definition(m *message) {
	s.answer(m, func(ix *index.Index, file string, p positionParams) ([]index.Location, error) {
		return ix.Definition(file, php.Position{Line: p.Position.Line, Character: p.Position.Character})
	})
}

func (s *server) references(m *message) {
	s.answer(m, func(ix *index.Index, file string, p positionParams) ([]index.Location, error) {
		pos := php.Position{Line: p.Position.Line, Character: p.Position.Character}
		return ix.References(file, pos, p.Context.IncludeDeclaration)
	})
}

// answer waits until the index is ready and answers a position request from it: an answer is never given from
// an index that is not there.
func (s *server) answer(m *message, query func(*index.Index, string, positionParams) ([]index.Location, error)) {
	var p positionParams
	if err := json.Unmarshal(m.Params, &p); err != nil {
		s.out.fail(m.ID, codeInvalidParams, m.Method+": "+err.Error())
		return
	}
	<-s.ready
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
