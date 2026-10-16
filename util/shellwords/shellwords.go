// Package shellwords provides POSIX shell-style word splitting, replacing the
// kballard/go-shellquote and google/shlex dependencies with mvdan.cc/sh/v3.
package shellwords

import (
	"fmt"
	"strings"

	"mvdan.cc/sh/v3/expand"
	"mvdan.cc/sh/v3/syntax"
)

var parser = syntax.NewParser()

// Split splits the given command line string into words according to POSIX
// shell word-splitting rules, removing quotes and resolving backslash escapes.
//
// No variable or parameter expansion is performed: the environment is empty, so
// an unquoted "$var" expands to the empty string, and command or process
// substitution results in an error.
func Split(s string) ([]string, error) {
	f, err := parser.Parse(strings.NewReader(s), "")
	if err != nil {
		return nil, err
	}
	var words []*syntax.Word
	for _, stmt := range f.Stmts {
		call, ok := stmt.Cmd.(*syntax.CallExpr)
		if !ok {
			return nil, fmt.Errorf("expected a simple command, got %T", stmt.Cmd)
		}
		words = append(words, call.Args...)
	}
	return expand.Fields(&expand.Config{}, words...)
}
