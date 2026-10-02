# Persisting compiled programs

A service that always compiles the same set of expressions on every restart
pays the parse, check and compile cost repeatedly. `expr` can instead save a
compiled program to disk once and load it on every startup.

The on-disk artifact is deliberately **pointer-free**: it stores bytecode,
arguments, stable constants, variable layout and function symbols (name plus
declared type signature), but never Go pointers such as host functions, the
AST node or profiling spans. On load, host functions are re-bound by stable
symbol name.

```go
program, err := expr.Compile(`Score >= discount(10)`,
    expr.Env(Env{}),
    expr.Function("discount", discount, new(func(int) int)))

data, err := expr.SaveProgram(program) // write data to disk

// another process, later:
program, err = expr.LoadProgram(data, expr.Hosts{
    "discount": {Fn: discount, Signature: "func(int) int"},
}, env)
out, err := expr.Run(program, env)
```

## Verification before execution

Deserialization alone is never enough to make a program runnable.
`LoadProgram` accepts an artifact only after all of the following pass:

1. **Container integrity** — magic, fixed header, exact payload length and a
   SHA-256 digest over the payload (truncation, trailing garbage and flipped
   bytes fail here).
2. **Format version** — the container format revision must match.
3. **Bytecode revision** — a hash of the opcode table (and builtin table)
   embedded at save time must match this binary, so adding, removing or
   reordering an opcode invalidates old artifacts.
4. **Instructions** — every opcode must exist; constant, function, builtin
   and variable references must be in range and point at constants of the
   expected type (`*runtime.Field`, `*regexp.Regexp`, ...).
5. **Jump targets** — every forward/backward jump must land inside the
   program.
6. **Stack usage** — a dataflow analysis proves no instruction pops more
   values than some real path can have pushed (underflow), and a linear
   program ends with a scalar result.
7. **Host bindings** — every non-builtin function symbol must be supplied
   with the standard `func(...any) (any, error)` convention and, when the
   host declares a signature, one matching the signature recorded at compile
   time. Struct field and method indexes recorded for a typed environment are
   re-checked against the environment passed to `LoadProgram`.

Each failure is reported with a dedicated, matchable sentinel error (`errors.Is`):

| Sentinel | Meaning |
| --- | --- |
| `vm.ErrArtifactTruncated` | container cut short or trailing bytes |
| `vm.ErrArtifactChecksum` | payload digest mismatch |
| `vm.ErrArtifactFormat` | bad magic or unsupported format version |
| `vm.ErrArtifactBytecode` | opcode/builtin revision mismatch |
| `vm.ErrArtifactProgram` | section decoded but structurally invalid |
| `vm.ErrArtifactInstruction` | unknown opcode or bad argument/reference |
| `vm.ErrArtifactJump` | jump leaves the program |
| `vm.ErrArtifactStack` | underflow or non-scalar terminal stack |
| `vm.ErrArtifactBinding` | missing/conflicting host function or moved field/method |

Programs compiled with profiling enabled cannot be saved, because their
bytecode references in-process `*Span` pointers.

## Two-process demo

`cmd/rules-save` compiles a fixed set of rules and writes pointer-free
artifacts to a cache directory; `cmd/rules-load` is a separate process that
loads and runs them without recompiling and prints the specific rejection
reason for corrupted bytecode, a truncated constant section, a bad jump, a
stack underflow and a changed host function signature.
