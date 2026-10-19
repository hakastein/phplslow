// Command phplslow is a PHP language server. A client starts it and speaks LSP over stdin and stdout.
package main

import (
	"os"

	"github.com/hakastein/phplslow/internal/lsp"
)

func main() {
	os.Exit(lsp.Serve(os.Stdin, os.Stdout, os.Stderr))
}
