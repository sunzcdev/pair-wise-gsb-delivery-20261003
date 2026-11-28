# frozen_string_literal: true
#--
# typed: true
require "securerandom"
require "graphql/subscriptions/broadcast_analyzer"
require "graphql/subscriptions/event"
require "graphql/subscriptions/serialize"
require "graphql/subscriptions/action_cable_subscriptions"
require "graphql/subscriptions/default_subscription_resolve_extension"

module GraphQL
  class Subscriptions
    # Raised when either:
    # - the triggered `event_name` doesn't match a field in the schema; or
    # - one or more arguments don't match the field arguments
    class InvalidTriggerError < GraphQL::Error
    end

    # Raised when either:
    # - An initial subscription didn't have a value for `context[subscription_scope]`
    # - Or, an update didn't pass `.trigger(..., scope:)`
    # When raised, the initial subscription or update fails completely.
    class SubscriptionScopeMissingError < GraphQL::Error
    end

    # A registry of {GraphQL::Subscriptions} instances by the `schema_version:`
    # they were configured with. When two versions of a schema run side-by-side
    # (for example, while old subscriptions are still receiving updates),
    # persisted subscription data remembers which version it was validated with,
    # and this registry routes updates to the instance for that version --
    # even if the new schema reuses the same field names.
    #
    # **Returns**
    #
    # - `Hash{Object => GraphQL::Subscriptions}` — Registered instances, keyed by schema version
    def self.schema_version_registry
      if self.equal?(GraphQL::Subscriptions)
        @schema_version_registry ||= {}
      else
        GraphQL::Subscriptions.schema_version_registry
      end
    end

    # Register `subscriptions` as the manager for `schema_version`.
    # (This happens automatically when a subscriptions instance is created with `schema_version:`.)
    #
    # **Parameters**
    #
    # - `version` (`Object`) — The version identifier, eg `"v1"` or a `String` digest
    # - `subscriptions` (`GraphQL::Subscriptions`) — The instance managing subscriptions for that version
    #
    # **Returns**
    #
    # - `void`
    def self.register_schema_version(version, subscriptions)
      schema_version_registry[version] = subscriptions
      nil
    end

    # **Parameters**
    #
    # - `version` (`Object`) — A previously-registered schema version
    #
    # **Returns**
    #
    # - `GraphQL::Subscriptions, nil` — The instance registered for `version`, if any
    def self.subscriptions_for_schema_version(version)
      schema_version_registry[version]
    end

    # Retire `version`: from now on, updates for subscriptions that were
    # validated with `version` will be terminated instead of executed.
    # See [terminate_retired_subscription](rdoc-ref:#terminate_retired_subscription).
    #
    # **Parameters**
    #
    # - `version` (`Object`) — The schema version to retire
    # - `reason` (`String, nil`) — A termination reason, passed through to `terminate_retired_subscription`
    #
    # **Returns**
    #
    # - `void`
    def self.retire_schema_version(version, reason: nil)
      retired_schema_versions[version] = reason || "Schema version #{version.inspect} was retired"
      nil
    end

    # **Parameters**
    #
    # - `version` (`Object`) — A schema version
    #
    # **Returns**
    #
    # - `String, nil` — The retirement reason for `version`, or `nil` if it hasn't been retired
    def self.retired_schema_version_reason(version)
      retired_schema_versions[version]
    end

    # Forget all registered and retired schema versions. (Mostly useful in tests.)
    #
    # **Returns**
    #
    # - `void`
    def self.reset_schema_versions!
      if self.equal?(GraphQL::Subscriptions)
        @schema_version_registry = {}
        @retired_schema_versions = {}
      else
        GraphQL::Subscriptions.reset_schema_versions!
      end
      nil
    end

    # **Returns**
    #
    # - `Hash{Object => String}` — Retired schema versions and their termination reasons
    def self.retired_schema_versions
      if self.equal?(GraphQL::Subscriptions)
        @retired_schema_versions ||= {}
      else
        GraphQL::Subscriptions.retired_schema_versions
      end
    end

    # See [GraphQL::Subscriptions](rdoc-ref:GraphQL::Subscriptions) for the base options; concrete implementations may add options.
    def self.use(defn, options = {})
      schema = defn.is_a?(Class) ? defn : defn.target

      if schema.subscriptions(inherited: false)
        raise ArgumentError, "Can't reinstall subscriptions. #{schema} is using #{schema.subscriptions}, can't also add #{self}"
      end

      options[:schema] = schema
      schema.subscriptions = self.new(**options)
      schema.add_subscription_extension_if_necessary
      nil
    end

    # **Parameters**
    #
    # - `schema` (`Class`) — the GraphQL schema this manager belongs to
    # - `validate_update` (`Boolean`) — If false, then validation is skipped when executing updates
    # - `schema_version` (`Object`) — An identifier for the version of `schema` this instance serves.
    #   When two versions of a schema run in parallel, persisted subscription data remembers
    #   the version it was validated with, so updates are executed by the instance
    #   registered for that version instead of being migrated to a new schema
    #   that happens to reuse the same field names.
    #
    # :call-seq:
    #   initialize(Class schema:, bool validate_update:, broadcast:, default_broadcastable:, schema_version:, **rest)
    def initialize(schema:, validate_update: true, broadcast: false, default_broadcastable: false, schema_version: nil, **rest)
      if broadcast
        schema.query_analyzer(Subscriptions::BroadcastAnalyzer)
      end
      @default_broadcastable = default_broadcastable
      @schema = schema
      @validate_update = validate_update
      @schema_version = schema_version
      if !schema_version.nil?
        GraphQL::Subscriptions.register_schema_version(schema_version, self)
      end
    end

    # **Returns**
    #
    # - `Object` — The version identifier this instance was configured with, if any
    attr_reader :schema_version

    # **Returns**
    #
    # - `Boolean` — Used when fields don't have `broadcastable:` explicitly set
    attr_reader :default_broadcastable

    # Fetch subscriptions matching this field + arguments pair
    # And pass them off to the queue.
    #
    # **Parameters**
    #
    # - `event_name` (`String`)
    # - `args` (`Hash<String, Symbol => Object>`) — Arguments passed to the subscription resolver
    # - `object` (`Object`)
    # - `scope` (`Symbol, String`)
    # - `context` (`Hash`)
    #
    # **Returns**
    #
    # - `void`
    #
    # :call-seq:
    #   trigger(String event_name, Hash[String | Symbol, Object] args, Object object, Symbol | String scope:, Hash context:) -> void
    def trigger(event_name, args, object, scope: nil, context: {})
      # Make something as context-like as possible, even though there isn't a current query:
      dummy_query = @schema.query_class.new(@schema, "{ __typename }", validate: false, context: context)
      context = dummy_query.context
      event_name = event_name.to_s

      # Try with the verbatim input first:
      field = dummy_query.types.field(@schema.subscription, event_name) # rubocop:disable Development/ContextIsPassedCop

      if field.nil?
        # And if it wasn't found, normalize it:
        normalized_event_name = normalize_name(event_name)
        field = dummy_query.types.field(@schema.subscription, normalized_event_name) # rubocop:disable Development/ContextIsPassedCop
        if field.nil?
          raise InvalidTriggerError, "No subscription matching trigger: #{event_name} (looked for #{@schema.subscription.graphql_name}.#{normalized_event_name})"
        end
      else
        # Since we found a field, the original input was already normalized
        normalized_event_name = event_name
      end

      # Normalize symbol-keyed args to strings, try camelizing them
      # Should this accept a real context somehow?
      normalized_args = normalize_arguments(normalized_event_name, field, args, @schema.null_context)

      event = Subscriptions::Event.new(
        name: normalized_event_name,
        arguments: normalized_args,
        field: field,
        scope: scope,
        context: context,
      )
      execute_all(event, object)
    end

    # `event` was triggered on `object`, and `subscription_id` was subscribed,
    # so it should be updated.
    #
    # Load `subscription_id`'s GraphQL data, re-evaluate the query and return the result.
    #
    # **Parameters**
    #
    # - `subscription_id` (`String`)
    # - `event` (`GraphQL::Subscriptions::Event`) — The event which was triggered
    # - `object` (`Object`) — The value for the subscription field
    #
    # **Returns**
    #
    # - `GraphQL::Query::Result`
    #
    # :call-seq:
    #   execute_update(String subscription_id, GraphQL::Subscriptions::Event event, Object object) -> GraphQL::Query::Result
    def execute_update(subscription_id, event, object)
      # Lookup the saved data for this subscription
      query_data = read_subscription(subscription_id)
      if query_data.nil?
        delete_subscription(subscription_id)
        return nil
      end

      stored_version = query_data[:schema_version]
      if !stored_version.nil? && stored_version != @schema_version
        # This subscription was validated with another version of the schema.
        # Don't migrate it to this schema -- even if this schema reuses the
        # same field names -- instead, hand it off to the instance registered
        # for its own version, or terminate it if that version was retired.
        if (retirement_reason = GraphQL::Subscriptions.retired_schema_version_reason(stored_version))
          terminate_retired_subscription(subscription_id, stored_version, retirement_reason)
          return nil
        elsif (version_owner = GraphQL::Subscriptions.subscriptions_for_schema_version(stored_version))
          return version_owner.execute_update(subscription_id, event, object)
        else
          # No instance is registered for that version in this process;
          # leave the subscription alone so its own process can update it.
          return nil
        end
      elsif !@schema_version.nil? && (retirement_reason = GraphQL::Subscriptions.retired_schema_version_reason(@schema_version))
        # This instance's own version was retired; terminate its subscriptions.
        terminate_retired_subscription(subscription_id, @schema_version, retirement_reason)
        return nil
      end

      # Fetch the required keys from the saved data
      query_string = query_data.fetch(:query_string)
      variables = query_data.fetch(:variables)
      context = query_data.fetch(:context)
      operation_name = query_data.fetch(:operation_name)
      execute_options = {
        query: query_string,
        context: context,
        subscription_topic: event.topic,
        operation_name: operation_name,
        variables: variables,
        root_value: object,
      }

       # merge event's and query's context together
      context.merge!(event.context) unless event.context.nil? || context.nil?

      execute_options[:validate] = validate_update?(**execute_options)
      result = @schema.execute(**execute_options)
      subscriptions_context = result.context.namespace(:subscriptions)
      # Tag the result with the version that produced it:
      subscriptions_context[:schema_version] = @schema_version
      if subscriptions_context[:no_update]
        result = nil
      end

      if subscriptions_context[:unsubscribed] && !subscriptions_context[:final_update]
        # `unsubscribe` was called, clean up on our side
        # The transport should also send `{more: false}` to client
        delete_subscription(subscription_id)
        result = nil
      end

      result
    end

    # Define this method to customize whether to validate
    # this subscription when executing an update.
    #
    # **Returns**
    #
    # - `Boolean` — defaults to `true`, or false if `validate: false` is provided.
    #
    # :call-seq:
    #   validate_update?(query:, context:, root_value:, subscription_topic:, operation_name:, variables:) -> bool
    def validate_update?(query:, context:, root_value:, subscription_topic:, operation_name:, variables:)
      @validate_update
    end

    # Run the update query for this subscription and deliver it
    # See [execute_update](rdoc-ref:#execute_update)
    # See [deliver](rdoc-ref:#deliver)
    #
    # **Returns**
    #
    # - `void`
    #
    # :call-seq:
    #   execute(subscription_id, event, object) -> void
    def execute(subscription_id, event, object)
      res = execute_update(subscription_id, event, object)
      if !res.nil?
        deliver(subscription_id, res)

        if res.context.namespace(:subscriptions)[:unsubscribed]
          # `unsubscribe` was called, clean up on our side
          # The transport should also send `{more: false}` to client
          delete_subscription(subscription_id)
        end
      end

    end

    # Event `event` occurred on `object`,
    # Update all subscribers.
    #
    # **Parameters**
    #
    # - `event` (`Subscriptions::Event`)
    # - `object` (`Object`)
    #
    # **Returns**
    #
    # - `void`
    #
    # :call-seq:
    #   execute_all(Subscriptions::Event event, Object object) -> void
    def execute_all(event, object)
      raise GraphQL::RequiredImplementationMissingError
    end

    # The system wants to send an update to this subscription.
    # Read its data and return it.
    #
    # **Parameters**
    #
    # - `subscription_id` (`String`)
    #
    # **Returns**
    #
    # - `Hash` — Containing required keys
    #
    # :call-seq:
    #   read_subscription(String subscription_id) -> Hash
    def read_subscription(subscription_id)
      raise GraphQL::RequiredImplementationMissingError
    end

    # A subscription query was re-evaluated, returning `result`.
    # The result should be send to `subscription_id`.
    #
    # **Parameters**
    #
    # - `subscription_id` (`String`)
    # - `result` (`Hash`)
    #
    # **Returns**
    #
    # - `void`
    #
    # :call-seq:
    #   deliver(String subscription_id, Hash result) -> void
    def deliver(subscription_id, result)
      raise GraphQL::RequiredImplementationMissingError
    end

    # `query` was executed and found subscriptions to `events`.
    # Update the database to reflect this new state.
    #
    # **Parameters**
    #
    # - `query` (`GraphQL::Query`)
    # - `events` (`Array<GraphQL::Subscriptions::Event>`)
    #
    # **Returns**
    #
    # - `void`
    #
    # :call-seq:
    #   write_subscription(GraphQL::Query query, Array[GraphQL::Subscriptions::Event] events) -> void
    def write_subscription(query, events)
      raise GraphQL::RequiredImplementationMissingError
    end

    # A subscription was terminated server-side.
    # Clean up the database.
    #
    # **Parameters**
    #
    # - `subscription_id` (`String`)
    #
    # **Returns**
    #
    # - `Object` — void.
    #
    # :call-seq:
    #   delete_subscription(String subscription_id) -> Object
    def delete_subscription(subscription_id)
      raise GraphQL::RequiredImplementationMissingError
    end

    # Called when an update arrives for a subscription whose persisted
    # `schema_version` has been retired (see [retire_schema_version](rdoc-ref:.retire_schema_version)).
    # By default, the subscription is deleted. Override this to notify the
    # client first -- `version` and `reason` identify the retired schema version
    # that this subscription was validated with.
    #
    # **Parameters**
    #
    # - `subscription_id` (`String`)
    # - `version` (`Object`) — The retired schema version the subscription belongs to
    # - `reason` (`String`) — The reason given when the version was retired
    #
    # **Returns**
    #
    # - `void`
    #
    # :call-seq:
    #   terminate_retired_subscription(String subscription_id, Object version, String reason) -> void
    def terminate_retired_subscription(subscription_id, version, reason)
      delete_subscription(subscription_id)
      nil
    end

    # **Returns**
    #
    # - `String` — A new unique identifier for a subscription
    #
    # :call-seq:
    #   build_id() -> String
    def build_id
      SecureRandom.uuid
    end

    # Convert a user-provided event name or argument
    # to the equivalent in GraphQL.
    #
    # By default, it converts the identifier to camelcase.
    # Override this in a subclass to change the transformation.
    #
    # **Parameters**
    #
    # - `event_or_arg_name` (`String, Symbol`)
    #
    # **Returns**
    #
    # - `String`
    #
    # :call-seq:
    #   normalize_name(String | Symbol event_or_arg_name) -> String
    def normalize_name(event_or_arg_name)
      Schema::Member::BuildType.camelize(event_or_arg_name.to_s)
    end

    # **Returns**
    #
    # - `Boolean` — if true, then a query like this one would be broadcasted
    #
    # :call-seq:
    #   broadcastable?(query_str, **query_options) -> bool
    def broadcastable?(query_str, **query_options)
      query = @schema.query_class.new(@schema, query_str, **query_options)
      if !query.valid?
        raise "Invalid query: #{query.validation_errors.map(&:to_h).inspect}"
      end
      GraphQL::Analysis.analyze_query(query, @schema.query_analyzers)
      query.context.namespace(:subscriptions)[:subscription_broadcastable]
    end

    # Called during execution when a new `subscription ...` operation is received
    #
    # **Parameters**
    #
    # - `query` (`GraphQL::Query`)
    #
    # **Returns**
    #
    # - `void`
    #
    # :call-seq:
    #   initialize_subscriptions(GraphQL::Query query) -> void
    def initialize_subscriptions(query)
      subs_namespace = query.context.namespace(:subscriptions)
      subs_namespace[:events] = []
      subs_namespace[:subscriptions] = {}
      # Remember which schema version this subscription is validated with,
      # so that persisted data can be routed to the right version later:
      subs_namespace[:schema_version] = @schema_version
      nil
    end

    # Called during execution when a subscription operation has finished
    #
    # **Parameters**
    #
    # - `query` (`GraphQL::Query`)
    #
    # **Returns**
    #
    # - `void`
    #
    # :call-seq:
    #   finish_subscriptions(GraphQL::Query query) -> void
    def finish_subscriptions(query)
      if (events = query.context.namespace(:subscriptions)[:events]) && !events.empty?
        write_subscription(query, events)
      end
      nil
    end

    def finalizer
      Finalizer.new(self)
    end

    class Finalizer
      include Execution::Finalizer
      def initialize(subscriptions)
        @subscriptions = subscriptions
      end

      def finalize_graphql_result(query, result_data, result_key)
        @subscriptions.finish_subscriptions(query)
      end
    end

    private

    # Recursively normalize `args` as belonging to `arg_owner`:
    # - convert symbols to strings,
    # - if needed, camelize the string (using [normalize name](rdoc-ref:#normalize_name))
    #
    # **Parameters**
    #
    # - `arg_owner` (`GraphQL::Field, GraphQL::BaseType`)
    # - `args` (`Hash, Array, Any`) — some GraphQL input value to coerce as `arg_owner`
    #
    # **Returns**
    #
    # - `Any` — normalized arguments value
    #
    # :call-seq:
    #   normalize_arguments(event_name, GraphQL::Field | GraphQL::BaseType arg_owner, Hash | Array | Any args, context) -> Any
    def normalize_arguments(event_name, arg_owner, args, context)
      case arg_owner
      when GraphQL::Schema::Field, Class
        return args if args.nil?

        arg_owner = arg_owner #: as GraphQL::Schema::Field | GraphQL::Schema::Member::type_definition
        if arg_owner.is_a?(Class) && !arg_owner.kind.input_object?
          # it's a type, but not an input object
          return args
        end
        arg_owner = arg_owner #: as GraphQL::Schema::Field | singleton(GraphQL::Schema::InputObject)
        normalized_args = {}
        missing_arg_names = []
        args.each do |k, v|
          arg_name = k.to_s
          arg_defn = arg_owner.get_argument(arg_name, context)
          if arg_defn
            normalized_arg_name = arg_name
          else
            normalized_arg_name = normalize_name(arg_name)
            arg_defn = arg_owner.get_argument(normalized_arg_name, context)
          end

          if arg_defn
            if arg_defn.loads
              normalized_arg_name = arg_defn.keyword.to_s
            end
            normalized = normalize_arguments(event_name, arg_defn.type, v, context)
            normalized_args[normalized_arg_name] = normalized
          else
            # Couldn't find a matching argument definition
            missing_arg_names << arg_name
          end
        end

        # Backfill default values so that trigger arguments
        # match query arguments.
        arg_owner.arguments(context).each do |_name, arg_defn|
          if arg_defn.default_value? && !normalized_args.key?(arg_defn.name)
            default_value = arg_defn.default_value
            # We don't have an underlying "object" here, so it can't call methods.
            # This is broken.
            normalized_args[arg_defn.name] = arg_defn.prepare_value(nil, default_value, context: context)
          end
        end

        if !missing_arg_names.empty?
          arg_owner_name = if arg_owner.is_a?(GraphQL::Schema::Field)
            arg_owner.path
          else
            arg_owner.graphql_name
          end
          raise InvalidTriggerError, "Can't trigger Subscription.#{event_name}, received undefined arguments: #{missing_arg_names.join(", ")}. (Should match arguments of #{arg_owner_name}.)"
        end

        normalized_args
      when GraphQL::Schema::List
        args&.map { |a| normalize_arguments(event_name, arg_owner.of_type, a, context) }
      when GraphQL::Schema::NonNull
        normalize_arguments(event_name, arg_owner.of_type, args, context)
      else
        args
      end
    end
  end
end
