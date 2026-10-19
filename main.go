// Command phplslow is a PHP language server. A client starts it and speaks LSP over stdin and stdout.
package main

import (
	"os"
	"os/signal"
	"syscall"

	"github.com/hakastein/phplslow/internal/lsp"
)

func main() {
	// A write to a closed stdout then fails with an error the server handles instead of killing the process.
	signal.Ignore(syscall.SIGPIPE)
	os.Exit(lsp.Serve(os.Stdin, os.Stdout, os.Stderr))
}
