package vm

import (
	"fmt"
	"reflect"

	"github.com/expr-lang/expr/builtin"
	"github.com/expr-lang/expr/vm/runtime"
)

var standardHostType = reflect.TypeOf(func(...any) (any, error) { return nil, nil })

// bindHosts re-attaches host-provided function pointers after verification.
// Symbols that name a builtin are satisfied by the builtin table bundled with
// this binary; every other symbol must be present in hosts with a Fn that has
// the standard calling convention and, when a signature is supplied, a
// signature matching the one recorded at compile time.
func bindHosts(program *Program, hosts Hosts, env any) error {
	names, sigs, kinds := program.functionNames, program.functionSigs, program.functionKinds
	for i := range names {
		name := names[i]
		if name == "" {
			return fmt.Errorf("%w: function slot %d has an empty symbol name", ErrArtifactBinding, i)
		}
		if kinds[i] == FunctionKindBuiltin {
			id, ok := builtin.Index[name]
			if !ok {
				return fmt.Errorf("%w: builtin symbol %q does not exist in this binary",
					ErrArtifactBinding, name)
			}
			f := builtin.Builtins[id]
			if f == nil || f.Func == nil {
				return fmt.Errorf("%w: builtin %q is not a host-callable function in this binary",
					ErrArtifactBinding, name)
			}
			program.functions[i] = f.Func
			continue
		}

		host, ok := hosts[name]
		if !ok {
			return fmt.Errorf("%w: no host implementation provided for function symbol %q (slot %d)",
				ErrArtifactBinding, name, i)
		}
		if host.Fn == nil {
			return fmt.Errorf("%w: host implementation for %q is nil", ErrArtifactBinding, name)
		}
		if got := reflect.TypeOf(host.Fn); got != standardHostType {
			return fmt.Errorf("%w: function %q has runtime type %s, expected %s",
				ErrArtifactBinding, name, got, standardHostType)
		}
		if host.Signature != "" && sigs[i] != "" && host.Signature != sigs[i] {
			return fmt.Errorf("%w: function %q signature mismatch: artifact records %q, host declares %q",
				ErrArtifactBinding, name, sigs[i], host.Signature)
		}
		program.functions[i] = host.Fn
	}

	if env != nil {
		if err := verifyEnvBindings(program, env); err != nil {
			return err
		}
	}
	return nil
}

// verifyEnvBindings checks that struct field and method indexes recorded at
// compile time still point at identically named members of env. Only
// top-level identifiers can be resolved against env; chained accesses are
// validated structurally and remain safe because FetchField bounds-checks
// every index at runtime.
func verifyEnvBindings(program *Program, env any) error {
	t := reflect.TypeOf(env)
	for t != nil && t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	if t == nil || t.Kind() != reflect.Struct {
		// map and dynamic environments are accessed by stable string key,
		// there is no baked-in index to verify.
		return nil
	}

	for i, c := range program.Constants {
		switch x := c.(type) {
		case *runtime.Field:
			// Path[0] is the top-level identifier. For nested chains the
			// compiler emits one Field per hop, each rooted at its own type,
			// so only single-entry paths are verifiable against env here.
			if len(x.Path) != 1 {
				continue
			}
			f, ok := t.FieldByName(x.Path[0])
			if !ok {
				return fmt.Errorf("%w: environment has no field %q used by constant #%d",
					ErrArtifactBinding, x.Path[0], i)
			}
			if !indexEqual(f.Index, x.Index) {
				return fmt.Errorf("%w: field %q moved: artifact index %v, host struct index %v",
					ErrArtifactBinding, x.Path[0], x.Index, f.Index)
			}
		case *runtime.Method:
			m, ok := t.MethodByName(x.Name)
			if !ok {
				return fmt.Errorf("%w: environment has no method %q used by constant #%d",
					ErrArtifactBinding, x.Name, i)
			}
			if m.Index != x.Index {
				return fmt.Errorf("%w: method %q moved: artifact index %d, host struct index %d",
					ErrArtifactBinding, x.Name, x.Index, m.Index)
			}
		}
	}
	return nil
}

func indexEqual(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
