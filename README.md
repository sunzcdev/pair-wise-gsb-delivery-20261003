<!--- this file is synced from dry-rb/template-gem project -->
[gem]: https://rubygems.org/gems/dry-container
[actions]: https://github.com/dry-rb/dry-container/actions

# dry-container [![Gem Version](https://badge.fury.io/rb/dry-container.svg)][gem] [![CI Status](https://github.com/dry-rb/dry-container/workflows/ci/badge.svg)][actions]

## Links

* [User documentation](https://dry-rb.org/gems/dry-container)
* [API documentation](http://rubydoc.info/gems/dry-container)
* [Forum](https://discourse.dry-rb.org)

## Supported Ruby versions

This library officially supports the following Ruby versions:

* MRI `>= 3.0.0`
* jruby `>= 9.4` (not tested on CI)

## Lifetimes and scopes

Registered items can be given one of three lifetimes:

* `lifetime: :resolution` (default for blocks) — a new value is built on every
  resolution.
* `lifetime: :application` (equivalent to `memoize: true`) — the value is built
  once and cached for the whole application.
* `lifetime: :scope` — the value is built once per scope and cached there. A
  scope-level item can only be resolved while a scope is active.

```ruby
container = Dry::Container.new

container.register(:current_user, lifetime: :scope) { CurrentUser.new(session[:user_id]) }

# Block form: the scope is closed automatically when the block returns (or
# raises). Plain `resolve` calls inside the block see the scope.
container.with_scope do
  container.resolve(:current_user) # cached for this scope only
end

# Explicit form: open a scope, resolve through it, and close it yourself.
scope = container.enter_scope(:request)
scope.resolve(:current_user)
scope.close!
```

Caches never leak across scopes or threads: each thread keeps its own stack of
ambient scopes, so two interleaved requests on the same thread stay isolated,
and a closed scope can never be reactivated (resolving through a closed scope
raises `Dry::Container::ScopeClosedError`).

An application-level singleton is not allowed to capture a scope-level object,
since that would pin one request's value into a long-lived dependency. The
error is raised while the object is being constructed and names the full
construction chain:

```
Dry::Container::CaptiveDependencyError: application-level item "service"
captures scope-level dependency "current_user"; scope-level objects only live
for one scope and must not be held as long-lived dependencies
(construction chain: service -> current_user)
```

## License

See `LICENSE` file.
