// Command rules-save compiles the rule expressions and writes their stable,
// pointer-free artifacts to disk. It is the independent "compiler" process in
// the save/load demonstration.
package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/expr-lang/expr"
	"github.com/expr-lang/expr/cmd/rulesdemo"
)

func main() {
	dir := "rules-cache"
	if len(os.Args) > 1 {
		dir = os.Args[1]
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fatal("create cache dir: %v", err)
	}

	for name, source := range rulesdemo.Rules {
		program, err := expr.Compile(source, rulesdemo.CompileOptions()...)
		if err != nil {
			fatal("compile %q: %v", name, err)
		}
		data, err := expr.SaveProgram(program)
		if err != nil {
			fatal("save %q: %v", name, err)
		}
		path := filepath.Join(dir, name+".rule")
		if err := os.WriteFile(path, data, 0o644); err != nil {
			fatal("write %q: %v", path, err)
		}
		fmt.Printf("compiled and saved %-11s %5d bytes  %s\n", name, len(data), source)
	}
	fmt.Printf("\nartifacts written to %s\\ — no Go pointers are stored, only bytecode,\n", dir)
	fmt.Println("stable constants, type information and function symbols.")
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "rules-save: "+format+"\n", args...)
	os.Exit(1)
}
