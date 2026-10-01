package delegation

// Matcher decides whether a concrete value is covered by a delegated scope
// value, in the same direction as casbin request matching: value is the
// concrete request (or child scope) value and pattern is the stored scope
// value. Match(value, pattern) == true means value falls within pattern.
//
// The matcher is used for two purposes:
//
//   - enforcement: whether a concrete request is covered by a grant's scope;
//   - grant-time narrowing: whether a child scope value is covered by its
//     parent's scope value.
//
// The default matcher is equality. Supply a casbin matcher such as
// util.KeyMatch2 when scopes are expressed with patterns, for example:
//
//	mgr, _ := delegation.NewManager(enforcer, adapter,
//	    delegation.WithResourceMatcher(func(v, p string) bool {
//	        return util.KeyMatch2(v, p)
//	    }))
type Matcher func(value string, pattern string) bool

// EqualMatcher is the default Matcher: a value is covered only by the exact
// same scope value.
func EqualMatcher(value string, pattern string) bool {
	return value == pattern
}
