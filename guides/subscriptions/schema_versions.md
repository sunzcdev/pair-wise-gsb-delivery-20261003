# Schema Versions

During a schema migration, two versions of a schema may run in parallel for a while: new clients subscribe with the new schema while old subscriptions -- validated with the previous schema -- are still receiving updates. If the new schema reuses the same field names (for example, a same-named field whose output type changed), old subscriptions must _not_ be migrated to the new schema when updates are executed.

To support this, `GraphQL::Subscriptions` can tag each subscription with a __schema version__ and route updates to the instance registered for that version.

## Registering versions

Pass `schema_version:` when installing subscriptions on each schema:

```ruby
class SchemaV1 < GraphQL::Schema
  # ...
  use GraphQL::Subscriptions::ActionCableSubscriptions, schema_version: "v1"
end

class SchemaV2 < GraphQL::Schema
  # ...
  use GraphQL::Subscriptions::ActionCableSubscriptions, schema_version: "v2"
end
```

Each instance registers itself in `GraphQL::Subscriptions.schema_version_registry`, keyed by its version.

## Persisted version

When a subscription is first validated, its version is stored in `query.context.namespace(:subscriptions)[:schema_version]`. Include it in your persisted subscription data (as `schema_version:`) so that updates can be routed later. (`GraphQL::Subscriptions::ActionCableSubscriptions#read_subscription` includes it automatically.)

## Routing updates

When `execute_update` reads persisted data whose `schema_version` belongs to a different registered instance, the update is handed off to that instance, so the subscription keeps being executed by the schema version it was validated with -- even when triggered through another version's instance. Persisted data _without_ a version is executed by the receiving instance, as before.

Each delivered result is tagged with the version that produced it in `result.context.namespace(:subscriptions)[:schema_version]`.

## Retiring a version

When an old schema version is taken out of service, retire it explicitly:

```ruby
GraphQL::Subscriptions.retire_schema_version("v1", reason: "v1 was replaced by v2")
```

From then on, updates for subscriptions persisted with `"v1"` are terminated instead of executed. By default, termination just deletes the subscription; to notify clients first, override `terminate_retired_subscription`:

```ruby
class MySubscriptions < GraphQL::Subscriptions
  def terminate_retired_subscription(subscription_id, version, reason)
    # e.g. push a final message to the client, naming the retired version and reason
    super # deletes the subscription
  end
end
```

Subscriptions belonging to versions that were _not_ retired keep receiving updates as usual.
