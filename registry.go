package redcon

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
)

// KeySpecKind identifies the strategy used to locate keys within the
// arguments of a command.
type KeySpecKind int

const (
	// KeySpecNone means the command does not declare extractable keys.
	KeySpecNone KeySpecKind = iota
	// KeySpecRange locates keys at fixed positions: starting at First,
	// up to and including Last, advancing by Step. A Last of -1 means
	// "up to the final argument". This covers MGET-style commands
	// (First=1, Last=-1, Step=1) and MSET-style key/value pairs
	// (First=1, Last=-1, Step=2).
	KeySpecRange
	// KeySpecNumKeys locates keys through a runtime-declared count: the
	// argument at NumKeysAt holds the number of keys, and that many keys
	// immediately follow it. This covers EVAL/ZUNIONSTORE-style commands.
	KeySpecNumKeys
)

// KeySpec describes how to extract the keys a command accesses from its
// arguments. All positions are 1-based and relative to the arguments
// that follow the command name, i.e. position 1 is Command.Args[1].
type KeySpec struct {
	Kind KeySpecKind
	// First is the position of the first key (KeySpecRange only).
	First int
	// Last is the inclusive position of the final key, or -1 for the
	// last argument (KeySpecRange only).
	Last int
	// Step is the stride between consecutive keys (KeySpecRange and
	// KeySpecNumKeys).
	Step int
	// NumKeysAt is the position of the argument that declares how many
	// keys follow it (KeySpecNumKeys only).
	NumKeysAt int
}

func (spec KeySpec) validate() error {
	switch spec.Kind {
	case KeySpecNone:
		return nil
	case KeySpecRange:
		if spec.First < 1 {
			return errors.New("redcon: key spec First must be >= 1")
		}
		if spec.Last != -1 && spec.Last < spec.First {
			return errors.New("redcon: key spec Last must be -1 or >= First")
		}
		if spec.Step < 1 {
			return errors.New("redcon: key spec Step must be >= 1")
		}
		return nil
	case KeySpecNumKeys:
		if spec.NumKeysAt < 1 {
			return errors.New("redcon: key spec NumKeysAt must be >= 1")
		}
		if spec.Step < 1 {
			return errors.New("redcon: key spec Step must be >= 1")
		}
		return nil
	default:
		return errors.New("redcon: unknown key spec kind")
	}
}

// CommandSpec describes a command for validation and metadata purposes.
// It does not implement the command; it only tells proxies and routers
// how the command behaves.
type CommandSpec struct {
	// Name is the canonical command name, case-insensitive.
	Name string
	// Aliases are alternative names that resolve to this spec.
	Aliases []string
	// Arity follows the Redis convention: a positive value requires
	// exactly that many arguments including the command name, a negative
	// value -N requires at least N arguments. Zero is invalid.
	Arity int
	// ReadOnly reports whether the command never modifies data.
	ReadOnly bool
	// Keys describes how to extract the accessed keys. Use the zero
	// value (KeySpecNone) for commands without extractable keys.
	Keys KeySpec
}

func (spec CommandSpec) validate() error {
	if spec.Name == "" {
		return errors.New("redcon: command spec requires a name")
	}
	if spec.Arity == 0 {
		return fmt.Errorf("redcon: command %q has invalid arity 0", spec.Name)
	}
	if err := spec.Keys.validate(); err != nil {
		return fmt.Errorf("redcon: command %q: %v", spec.Name, err)
	}
	return nil
}

// arityError reports an arity violation for a command.
func arityError(name string) error {
	return fmt.Errorf("ERR wrong number of arguments for '%s' command", name)
}

// checkArity validates the argument count of cmd against the spec.
func (spec CommandSpec) checkArity(cmd Command) error {
	n := len(cmd.Args)
	if spec.Arity > 0 && n != spec.Arity {
		return arityError(strings.ToLower(string(cmd.Args[0])))
	}
	if spec.Arity < 0 && n < -spec.Arity {
		return arityError(strings.ToLower(string(cmd.Args[0])))
	}
	return nil
}

// extractKeys returns the keys accessed by cmd according to the spec.
// The cmd must already have passed checkArity.
func (spec CommandSpec) extractKeys(cmd Command) ([][]byte, error) {
	keyspec := spec.Keys
	args := cmd.Args[1:]
	switch keyspec.Kind {
	case KeySpecNone:
		return nil, nil
	case KeySpecRange:
		last := keyspec.Last
		if last == -1 {
			last = len(args)
		}
		if last > len(args) || keyspec.First > len(args) {
			return nil, fmt.Errorf(
				"ERR command '%s' declares keys at positions %d..%d but only %d arguments were provided",
				strings.ToLower(string(cmd.Args[0])), keyspec.First, last, len(args))
		}
		if (last-keyspec.First+1)%keyspec.Step != 0 {
			return nil, fmt.Errorf(
				"ERR command '%s' has an incomplete key group: positions %d..%d are not a multiple of step %d",
				strings.ToLower(string(cmd.Args[0])), keyspec.First, last, keyspec.Step)
		}
		var keys [][]byte
		for i := keyspec.First - 1; i < last; i += keyspec.Step {
			keys = append(keys, args[i])
		}
		return keys, nil
	case KeySpecNumKeys:
		if keyspec.NumKeysAt > len(args) {
			return nil, arityError(strings.ToLower(string(cmd.Args[0])))
		}
		raw := args[keyspec.NumKeysAt-1]
		numkeys, err := strconv.ParseInt(string(raw), 10, 64)
		if err != nil {
			if errors.Is(err, strconv.ErrRange) {
				return nil, fmt.Errorf(
					"ERR command '%s' numkeys value '%s' overflows an integer",
					strings.ToLower(string(cmd.Args[0])), raw)
			}
			return nil, fmt.Errorf(
				"ERR command '%s' numkeys value '%s' is not an integer",
				strings.ToLower(string(cmd.Args[0])), raw)
		}
		if numkeys < 0 {
			return nil, fmt.Errorf(
				"ERR command '%s' numkeys must not be negative, got %d",
				strings.ToLower(string(cmd.Args[0])), numkeys)
		}
		avail := int64(len(args) - keyspec.NumKeysAt)
		if numkeys > avail ||
			(numkeys > 0 && (numkeys-1)*int64(keyspec.Step) >= avail) {
			return nil, fmt.Errorf(
				"ERR command '%s' declares %d keys but only %d arguments follow numkeys",
				strings.ToLower(string(cmd.Args[0])), numkeys, avail)
		}
		var keys [][]byte
		for i := int64(0); i < numkeys; i++ {
			keys = append(keys, args[keyspec.NumKeysAt+int(i)*keyspec.Step])
		}
		return keys, nil
	}
	return nil, nil
}

// Registry is an optional command registry. It validates commands
// against registered specs and extracts the keys they access, which is
// useful for proxies that need to route or check permissions before a
// command reaches its business handler. It does not implement any
// command itself.
type Registry struct {
	mu    sync.RWMutex
	specs map[string]*CommandSpec
}

// NewRegistry returns an empty command registry.
func NewRegistry() *Registry {
	return &Registry{specs: make(map[string]*CommandSpec)}
}

// Register adds a command spec and its aliases to the registry.
// It returns an error if the spec is malformed or if the name or any
// alias is already registered.
func (r *Registry) Register(spec CommandSpec) error {
	if err := spec.validate(); err != nil {
		return err
	}
	names := make([]string, 0, len(spec.Aliases)+1)
	names = append(names, strings.ToLower(spec.Name))
	for _, alias := range spec.Aliases {
		if alias == "" {
			return fmt.Errorf("redcon: command %q has an empty alias", spec.Name)
		}
		names = append(names, strings.ToLower(alias))
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, name := range names {
		if _, exists := r.specs[name]; exists {
			return fmt.Errorf("redcon: command %q is already registered", name)
		}
	}
	spec.Name = names[0]
	spec.Aliases = names[1:]
	stored := spec
	for _, name := range names {
		r.specs[name] = &stored
	}
	return nil
}

// Lookup returns the spec registered under name or any of its aliases.
// The returned spec always carries the canonical name.
func (r *Registry) Lookup(name string) (CommandSpec, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	spec, ok := r.specs[strings.ToLower(name)]
	if !ok {
		return CommandSpec{}, false
	}
	return *spec, true
}

// Validate checks the argument count of cmd against its registered
// spec. Unknown commands are not validated and return nil.
func (r *Registry) Validate(cmd Command) error {
	if len(cmd.Args) == 0 {
		return nil
	}
	r.mu.RLock()
	spec, ok := r.specs[strings.ToLower(string(cmd.Args[0]))]
	r.mu.RUnlock()
	if !ok {
		return nil
	}
	return spec.checkArity(cmd)
}

// Keys validates cmd and returns the keys it accesses according to its
// registered spec. Unknown commands and commands without a key spec
// return nil keys and no error. Malformed invocations (bad arity,
// negative or overflowing numkeys, out-of-range key declarations) are
// reported as errors.
func (r *Registry) Keys(cmd Command) ([][]byte, error) {
	if len(cmd.Args) == 0 {
		return nil, nil
	}
	r.mu.RLock()
	spec, ok := r.specs[strings.ToLower(string(cmd.Args[0]))]
	r.mu.RUnlock()
	if !ok {
		return nil, nil
	}
	if err := spec.checkArity(cmd); err != nil {
		return nil, err
	}
	return spec.extractKeys(cmd)
}

// IsReadOnly reports whether the named command is registered as
// read-only. Unknown commands report false.
func (r *Registry) IsReadOnly(name string) bool {
	spec, ok := r.Lookup(name)
	return ok && spec.ReadOnly
}

// Wrap returns a Handler that validates every command against the
// registry before delegating to next. Commands that fail validation
// are answered with an error and never reach next.
func (r *Registry) Wrap(next Handler) Handler {
	return HandlerFunc(func(conn Conn, cmd Command) {
		if len(cmd.Args) == 0 {
			next.ServeRESP(conn, cmd)
			return
		}
		r.mu.RLock()
		spec, ok := r.specs[strings.ToLower(string(cmd.Args[0]))]
		r.mu.RUnlock()
		if !ok {
			next.ServeRESP(conn, cmd)
			return
		}
		if err := spec.checkArity(cmd); err != nil {
			conn.WriteError(err.Error())
			return
		}
		if _, err := spec.extractKeys(cmd); err != nil {
			conn.WriteError(err.Error())
			return
		}
		next.ServeRESP(conn, cmd)
	})
}

// WrapFunc is like Wrap but accepts a handler function.
func (r *Registry) WrapFunc(next func(conn Conn, cmd Command)) Handler {
	return r.Wrap(HandlerFunc(next))
}
