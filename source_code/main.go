package main

import (
	"fmt"
	"os"

	"pbmt/internal/bootstrap"
)

func main() {
	if err := bootstrap.Run(); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
