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
	codeInternalError        = -32603
	codeServerNotInitialized = -32002
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

// writer sends whole messages; it is safe for concurrent use.
type writer struct {
	mu sync.Mutex
	w  io.Writer
}

func (w *writer) reply(id json.RawMessage, value any) error {
	return w.send(result{JSONRPC: "2.0", ID: id, Result: value})
}

func (w *writer) fail(id json.RawMessage, code int, text string) error {
	if id == nil {
		id = json.RawMessage("null")
	}
	return w.send(failure{JSONRPC: "2.0", ID: id, Error: responseError{Code: code, Message: text}})
}

func (w *writer) send(m any) error {
	body, err := json.Marshal(m)
	if err != nil {
		return err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, err := fmt.Fprintf(w.w, "Content-Length: %d\r\n\r\n", len(body)); err != nil {
		return err
	}
	_, err = w.w.Write(body)
	return err
}
