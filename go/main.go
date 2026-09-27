// sshmc — Midnight-Commander-style dual-pane file manager over live SSH
// sessions. Go port; see README.md for keys and architecture.
package main

import (
	"fmt"
	"os"
)

func main() {
	if err := NewUI().Run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
