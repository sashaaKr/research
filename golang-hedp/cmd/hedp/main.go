// Command hedp serves dynamic Helm release rendering over HTTP.
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/sashaakr/research/golang-hedp/internal/server"
)

func main() {
	if err := server.Run(context.Background(), os.Args, os.Getenv, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "hedp:", err)
		os.Exit(1)
	}
}
