package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// TestUsageListsEveryDispatchCommand keeps the hand-written usage() synopsis in
// lockstep with the dispatch switch: a command added to dispatch without a usage
// entry (or a stale usage entry with no command) fails here rather than shipping
// an inaccurate --help.
func TestUsageListsEveryDispatchCommand(t *testing.T) {
	dispatched := dispatchCommands(t)
	listed := usageCommands(t)

	for command := range dispatched {
		if !listed[command] {
			t.Errorf("dispatch command %q is not listed in usage()", command)
		}
	}
	for command := range listed {
		if !dispatched[command] {
			t.Errorf("usage() lists %q, which dispatch does not handle", command)
		}
	}
}

// dispatchCommands parses the case labels of the value switch in dispatch() so
// the set of commands is read from the source of truth, not restated here.
func dispatchCommands(t *testing.T) map[string]bool {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate the test source file")
	}
	mainPath := filepath.Join(filepath.Dir(thisFile), "main.go")

	fileSet := token.NewFileSet()
	file, err := parser.ParseFile(fileSet, mainPath, nil, 0)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}

	var dispatch *ast.FuncDecl
	for _, declaration := range file.Decls {
		if function, isFunction := declaration.(*ast.FuncDecl); isFunction && function.Name.Name == "dispatch" {
			dispatch = function
			break
		}
	}
	if dispatch == nil {
		t.Fatal("dispatch function not found in main.go")
	}

	commands := make(map[string]bool)
	ast.Inspect(dispatch, func(node ast.Node) bool {
		clause, isClause := node.(*ast.CaseClause)
		if !isClause {
			return true
		}
		for _, expression := range clause.List {
			literal, isLiteral := expression.(*ast.BasicLit)
			if !isLiteral || literal.Kind != token.STRING {
				continue
			}
			value, err := strconv.Unquote(literal.Value)
			if err != nil {
				t.Fatalf("unquote case label %q: %v", literal.Value, err)
			}
			commands[value] = true
		}
		return true
	})
	if len(commands) == 0 {
		t.Fatal("no case labels found in dispatch()")
	}
	return commands
}

// usageCommands captures usage() and returns the first token of each command
// line in the Commands section. Continuation and flag lines start with
// punctuation and are skipped.
func usageCommands(t *testing.T) map[string]bool {
	t.Helper()
	output := captureStderr(t, usage)

	commands := make(map[string]bool)
	inCommands := false
	for _, line := range strings.Split(output, "\n") {
		if strings.TrimSpace(line) == "Commands:" {
			inCommands = true
			continue
		}
		if !inCommands || !strings.HasPrefix(line, " ") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		first := fields[0]
		if strings.HasPrefix(first, "[") || strings.HasPrefix(first, "|") || strings.HasPrefix(first, "-") {
			continue
		}
		commands[first] = true
	}
	if len(commands) == 0 {
		t.Fatal("no command lines parsed from usage()")
	}
	return commands
}

func captureStderr(t *testing.T, action func()) string {
	t.Helper()
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatalf("create pipe: %v", err)
	}
	original := os.Stderr
	os.Stderr = writer
	defer func() { os.Stderr = original }()

	action()
	if err := writer.Close(); err != nil {
		t.Fatalf("close pipe writer: %v", err)
	}
	data, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("read captured stderr: %v", err)
	}
	return string(data)
}
