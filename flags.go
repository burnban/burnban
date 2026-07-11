package main

import (
	"errors"
	"flag"
	"fmt"
	"strings"
)

func parseCommandFlags(fs *flag.FlagSet, args []string) (help bool, err error) {
	err = fs.Parse(args)
	if errors.Is(err, flag.ErrHelp) {
		return true, nil
	}
	return false, err
}

func requireNoArgs(fs *flag.FlagSet) error {
	if fs.NArg() == 0 {
		return nil
	}
	quoted := make([]string, fs.NArg())
	for i, value := range fs.Args() {
		quoted[i] = fmt.Sprintf("%q", value)
	}
	return fmt.Errorf("unexpected positional argument(s): %s", strings.Join(quoted, ", "))
}
