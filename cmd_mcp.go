package main

import (
	"flag"
	"os"

	"github.com/burnban/burnban/internal/mcp"
	"github.com/burnban/burnban/internal/store"
)

func cmdMCP(args []string) error {
	fs := flag.NewFlagSet("mcp", flag.ExitOnError)
	dbPath := fs.String("db", defaultDBPath(), "sqlite database path")
	fs.Parse(args)

	s, err := store.Open(*dbPath)
	if err != nil {
		return err
	}
	defer s.Close()

	srv := &mcp.Server{S: s, Version: version, In: os.Stdin, Out: os.Stdout}
	return srv.Run()
}
