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

## License

See `LICENSE` file.

## Multibindings

Sometimes one extension point needs several handlers invoked in turn
(listeners, subscribers, middleware…). A single key resolves a single object,
which does not fit this use case. Multibindings let you register named
members of a collection and resolve either the whole ordered set or one
member.

```ruby
container = Dry::Container.new

container.bind(:event_handlers, :logging, ->(event) { log(event) })
container.bind(:event_handlers, :analytics, ->(event) { track(event) }, tags: :async)
container.bind(:event_handlers, :audit, ->(event) { audit(event) }, order: -1, memoize: true)

# Ordered array of resolved members
container.resolve_all(:event_handlers)

# Resolve one member directly
container.resolve("event_handlers.analytics")

# Only members carrying a tag; factories of filtered-out members never run
container.resolve_all(:event_handlers, :async)

# Filter on member metadata (name, tags, order, source) before resolving
container.resolve_all(:event_handlers) { |member| member.order && member.order < 0 }
```

Members each have:

* a stable, unique **name** within the collection;
* one or more **tags** (the collection key and the name are always included);
* an explicit **order** weight, otherwise registration order is used; the
  resulting order never depends on hash traversal;
* an optional **source** used in the error raised on a name conflict.

Members honour the usual `call:` and `memoize:` options independently.
Resolving a set and resolving a single member share the same underlying
member, so a memoized member is instantiated once:

```ruby
container.bind(:handlers, :a, -> { MyHandler.new }, memoize: true)

container.resolve_all(:handlers).first.equal?(container.resolve("handlers.a")) # => true
```

Collections are merged too: merging a container that defines the same
collection unions the members (raising on duplicate names, reporting both
sources), and merging into a namespace prefixes the collection key while
keeping member names stable:

```ruby
other = Dry::Container.new
other.bind(:handlers, :b) { "b" }

container.merge(other, namespace: "events")
container.resolve_all("events.handlers")
container.resolve("events.handlers.b")
```
