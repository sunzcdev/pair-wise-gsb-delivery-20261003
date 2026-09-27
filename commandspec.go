package redcon

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// KeySpec extracts the key arguments from a command's arguments.
// The args passed to a KeySpec do not include the command name.
// A KeySpec must return an error when the arguments do not satisfy
// the declared layout, so that malformed commands are rejected
// before they reach the business handler.
type KeySpec func(args [][]byte) (keys [][]byte, err error)

// NoKeys returns a KeySpec for commands that do not access any keys.
func NoKeys() KeySpec {
	return func(args [][]byte) ([][]byte, error) {
		return nil, nil
	}
}

// AllKeys returns a KeySpec in which every argument is a key,
// like MGET or DEL.
func AllKeys() KeySpec {
	return func(args [][]byte) ([][]byte, error) {
		return args, nil
	}
}

// OddKeys returns a KeySpec in which arguments at even indexes are
// keys and arguments at odd indexes are their values, like MSET.
// A dangling value without a matching key is rejected.
func OddKeys() KeySpec {
	return func(args [][]byte) ([][]byte, error) {
		if len(args)%2 != 0 {
			return nil, errors.New("unpaired key/value argument")
		}
		keys := make([][]byte, 0, len(args)/2)
		for i := 0; i < len(args); i += 2 {
			keys = append(keys, args[i])
		}
		return keys, nil
	}
}

// NumKeys returns a KeySpec that reads the number of keys from the
// argument at numKeysIndex and treats the numKeys arguments that
// immediately follow it as keys, like EVAL. Negative, non-numeric,
// overflowing, or out-of-range declarations are rejected.
// It panics if numKeysIndex is negative.
func NumKeys(numKeysIndex int) KeySpec {
	if numKeysIndex < 0 {
		panic("redcon: negative numkeys index")
	}
	return func(args [][]byte) ([][]byte, error) {
		if numKeysIndex >= len(args) {
			return nil, fmt.Errorf(
				"numkeys argument at index %d is missing", numKeysIndex)
		}
		numKeys, err := strconv.Atoi(string(args[numKeysIndex]))
		if err != nil {
			return nil, fmt.Errorf(
				"invalid numkeys declaration %q", args[numKeysIndex])
		}
		if numKeys < 0 {
			return nil, fmt.Errorf(
				"numkeys declaration %d is negative", numKeys)
		}
		if numKeys > len(args)-numKeysIndex-1 {
			return nil, fmt.Errorf(
				"numkeys declaration %d exceeds %d remaining arguments",
				numKeys, len(args)-numKeysIndex-1)
		}
		return args[numKeysIndex+1 : numKeysIndex+1+numKeys], nil
	}
}

// RangeKeys returns a KeySpec that selects the arguments at
// first, first+step, first+2*step, ... up to and including last.
// A negative last counts back from the final argument, so last=-1
// selects through the end of the command. Declarations that fall
// outside the actual arguments are rejected.
// It panics on an invalid spec declaration.
func RangeKeys(first, last, step int) KeySpec {
	if first < 0 {
		panic("redcon: negative first key index")
	}
	if step < 1 {
		panic("redcon: key step must be at least 1")
	}
	return func(args [][]byte) ([][]byte, error) {
		if first >= len(args) {
			return nil, fmt.Errorf(
				"first key index %d is out of range for %d arguments",
				first, len(args))
		}
		resolvedLast := last
		if resolvedLast < 0 {
			resolvedLast += len(args)
		}
		if resolvedLast >= len(args) {
			return nil, fmt.Errorf(
				"last key index %d is out of range for %d arguments",
				last, len(args))
		}
		var keys [][]byte
		for i := first; i <= resolvedLast; i += step {
			keys = append(keys, args[i])
		}
		return keys, nil
	}
}

// CommandSpec describes a command for validation and metadata
// purposes. It does not implement the command; it only tells a
// proxy or server how to check the command's shape and how to
// locate the keys it accesses.
type CommandSpec struct {
	// Name is the command name. Matching is case-insensitive.
	Name string
	// Aliases are alternative names for the same command.
	Aliases []string
	// Arity is the number of arguments including the command name.
	// A positive value requires an exact match, a negative value -N
	// requires at least N arguments. Zero is invalid.
	Arity int
	// ReadOnly reports whether the command only reads keys.
	ReadOnly bool
	// KeySpec locates the keys in the command arguments.
	// A nil KeySpec means the command accesses no keys.
	KeySpec KeySpec
}

// CommandRegistry is an optional registry of command specifications.
// It validates commands and extracts their keys before they reach a
// handler, and it answers metadata queries such as whether a command
// is read-only. It does not implement any command itself.
type CommandRegistry struct {
	specs map[string]*CommandSpec
}

// NewCommandRegistry returns an empty CommandRegistry.
func NewCommandRegistry() *CommandRegistry {
	return &CommandRegistry{specs: make(map[string]*CommandSpec)}
}

// Register adds a command specification to the registry.
// It returns an error if the specification is invalid or if the
// name or one of the aliases is already registered.
func (r *CommandRegistry) Register(spec CommandSpec) error {
	if spec.Name == "" {
		return errors.New("redcon: command spec has no name")
	}
	if spec.Arity == 0 {
		return fmt.Errorf("redcon: command %q has zero arity", spec.Name)
	}
	names := make([]string, 0, len(spec.Aliases)+1)
	names = append(names, spec.Name)
	names = append(names, spec.Aliases...)
	for _, name := range names {
		name = strings.ToLower(name)
		if name == "" {
			return fmt.Errorf("redcon: command %q has an empty alias",
				spec.Name)
		}
		if _, exists := r.specs[name]; exists {
			return fmt.Errorf("redcon: command %q already registered", name)
		}
	}
	registered := spec
	registered.Name = strings.ToLower(spec.Name)
	for _, name := range names {
		r.specs[strings.ToLower(name)] = &registered
	}
	return nil
}

// Lookup returns the specification registered for name, resolving
// aliases and case. The boolean reports whether name is known.
func (r *CommandRegistry) Lookup(name string) (*CommandSpec, bool) {
	spec, ok := r.specs[strings.ToLower(name)]
	return spec, ok
}

// Specs returns all registered specifications, sorted by name.
// Each specification is reported once, under its primary name.
func (r *CommandRegistry) Specs() []CommandSpec {
	seen := make(map[*CommandSpec]bool)
	var specs []CommandSpec
	for _, spec := range r.specs {
		if !seen[spec] {
			seen[spec] = true
			specs = append(specs, *spec)
		}
	}
	sort.Slice(specs, func(i, j int) bool { return specs[i].Name < specs[j].Name })
	return specs
}

// Validate checks that cmd names a registered command and satisfies
// its arity. It returns the resolved specification.
func (r *CommandRegistry) Validate(cmd Command) (*CommandSpec, error) {
	if len(cmd.Args) == 0 {
		return nil, errors.New("empty command")
	}
	name := strings.ToLower(string(cmd.Args[0]))
	spec, ok := r.specs[name]
	if !ok {
		return nil, fmt.Errorf("unknown command '%s'", name)
	}
	if err := spec.checkArity(len(cmd.Args)); err != nil {
		return nil, err
	}
	return spec, nil
}

func (spec *CommandSpec) checkArity(count int) error {
	if spec.Arity > 0 && count != spec.Arity ||
		spec.Arity < 0 && count < -spec.Arity {
		return fmt.Errorf(
			"wrong number of arguments for '%s' command", spec.Name)
	}
	return nil
}

// Keys validates cmd and returns the keys it accesses.
func (r *CommandRegistry) Keys(cmd Command) ([][]byte, error) {
	spec, err := r.Validate(cmd)
	if err != nil {
		return nil, err
	}
	if spec.KeySpec == nil {
		return nil, nil
	}
	return spec.KeySpec(cmd.Args[1:])
}

// Wrap returns a handler that validates each command against the
// registry and rejects invalid ones with an error reply before
// next is invoked. Unknown commands, arity mismatches, and key
// declarations that are negative, overflowing, or out of range
// never reach next.
func (r *CommandRegistry) Wrap(next HandlerFunc) HandlerFunc {
	return func(conn Conn, cmd Command) {
		if _, err := r.Validate(cmd); err != nil {
			conn.WriteError("ERR " + err.Error())
			return
		}
		if _, err := r.Keys(cmd); err != nil {
			conn.WriteError("ERR " + err.Error())
			return
		}
		next(conn, cmd)
	}
}
