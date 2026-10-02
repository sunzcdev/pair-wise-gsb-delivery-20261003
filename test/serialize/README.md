# Compiled program save/load demo

This demo shows how a rule service can avoid recompiling the same
expressions on every restart: one process compiles the rules and saves the
marshaled programs, another process loads, validates, re-binds, and runs
them.

- `host/` — the shared host environment: the `Env` type and the host
  functions (`isInternalIP`, `riskScore`) with their signatures.
- `save/` — the compile process. Compiles the rule set once and writes
  `rules.exbc`. The artifact contains only stable data: bytecode,
  arguments, pointer-free constants, and function symbols (name + type
  signature). No Go pointers are stored.
- `load/` — the serve process. Loads each program with `vm.Unmarshal`,
  which validates the format version, every instruction, every jump
  target, and the stack/scope usage, then re-binds host functions by name
  and verifies their signatures. Anything that fails is rejected before a
  single instruction executes.

## Run

```sh
go run ./test/serialize/save rules.exbc
go run ./test/serialize/load rules.exbc
```

## Rejection demos

Each of the following is rejected before execution with a distinguishable
error (see `vm.FormatError.Kind`):

```sh
# Corrupt an instruction: kind=opcode
go run ./test/serialize/load -tamper instruction rules.exbc

# Truncate the constants section: kind=truncated
go run ./test/serialize/load -tamper constants rules.exbc

# Re-bind against a host with a changed function signature: kind=signature
go run ./test/serialize/load -tamper signature rules.exbc
```
