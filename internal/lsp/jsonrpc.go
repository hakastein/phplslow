// Package lsp serves the Language Server Protocol over a byte stream: JSON-RPC 2.0 messages framed by
// Content-Length headers.
package lsp

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/textproto"
	"strconv"
	"sync"
)

// message is an incoming JSON-RPC message: a request has ID and Method, a notification only Method.
type message struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	bad    error           // the body is not a JSON-RPC message
}

type result struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result"`
}

type failure struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Error   responseError   `json:"error"`
}

type responseError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// JSON-RPC and LSP error codes.
const (
	codeParseError           = -32700
	codeInvalidRequest       = -32600
	codeMethodNotFound       = -32601
	codeInvalidParams        = -32602
	codeServerNotInitialized = -32002
	codeRequestCancelled     = -32800
	codeRequestFailed        = -32803
)

func read(r *bufio.Reader) (*message, error) {
	header, err := textproto.NewReader(r).ReadMIMEHeader()
	if err != nil {
		return nil, err
	}
	length, err := strconv.Atoi(header.Get("Content-Length"))
	if err != nil || length < 0 {
		return nil, fmt.Errorf("lsp: bad Content-Length %q", header.Get("Content-Length"))
	}
	body := make([]byte, length)
	if _, err := io.ReadFull(r, body); err != nil {
		return nil, err
	}
	var m message
	if err := json.Unmarshal(body, &m); err != nil {
		return &message{bad: err}, nil
	}
	return &m, nil
}

// writer sends whole messages; it is safe for concurrent use. After the first failed write it sends nothing more
// and failed is closed; err is the cause.
type writer struct {
	mu     sync.Mutex
	w      io.Writer
	err    error
	failed chan struct{}
}

func newWriter(w io.Writer) *writer {
	return &writer{w: w, failed: make(chan struct{})}
}

func (w *writer) reply(id json.RawMessage, value any) {
	w.send(result{JSONRPC: "2.0", ID: id, Result: value})
}

func (w *writer) fail(id json.RawMessage, code int, text string) {
	if id == nil {
		id = json.RawMessage("null")
	}
	w.send(failure{JSONRPC: "2.0", ID: id, Error: responseError{Code: code, Message: text}})
}

func (w *writer) send(m any) {
	body, err := json.Marshal(m)
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.err != nil {
		return
	}
	if err == nil {
		_, err = fmt.Fprintf(w.w, "Content-Length: %d\r\n\r\n", len(body))
	}
	if err == nil {
		_, err = w.w.Write(body)
	}
	if err != nil {
		w.err = err
		close(w.failed)
	}
}

// cause is the error of the failed write.
func (w *writer) cause() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.err
}
