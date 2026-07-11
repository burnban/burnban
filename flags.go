package main

import (
	"flag"
	"fmt"
	"strings"
)

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
