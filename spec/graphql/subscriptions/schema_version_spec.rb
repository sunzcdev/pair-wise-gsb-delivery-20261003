# frozen_string_literal: true
require "spec_helper"

describe "GraphQL::Subscriptions schema versions" do
  # A subscriptions backend whose storage is shared between several
  # schema versions running in parallel, like a shared Redis or database.
  class VersionedUpdateBackend < GraphQL::Subscriptions
    def initialize(storage:, **rest)
      @storage = storage
      super
    end

    def write_subscription(query, events)
      subscription_id = query.context[:subscription_id] ||= build_id
      @storage[:subscriptions][subscription_id] = {
        query_string: query.query_string,
        variables: query.provided_variables,
        operation_name: query.operation_name,
        context: {},
        # Persist the schema version this subscription was validated with:
        schema_version: query.context.namespace(:subscriptions)[:schema_version],
      }
      events.each do |ev|
        @storage[:events][ev.topic] << subscription_id
      end
    end

    def read_subscription(subscription_id)
      @storage[:subscriptions][subscription_id]
    end

    def delete_subscription(subscription_id)
      @storage[:subscriptions].delete(subscription_id)
      @storage[:events].each_value { |sub_ids| sub_ids.delete(subscription_id) }
    end

    def execute_all(event, object)
      @storage[:events][event.topic].dup.each do |subscription_id|
        execute(subscription_id, event, object)
      end
    end

    def deliver(subscription_id, result)
      @storage[:deliveries][subscription_id] << result
    end

    def terminate_retired_subscription(subscription_id, version, reason)
      @storage[:terminations] << { subscription_id: subscription_id, version: version, reason: reason }
      super
    end
  end

  # The same object is triggered for both schema versions;
  # each version reads a different method for its same-named `str` field.
  class VersionedObject
    attr_reader :str, :str_int

    def initialize(str, str_int)
      @str = str
      @str_int = str_int
    end
  end

  class VersionedPayloadUpdatedV1 < GraphQL::Schema::Subscription
    field :str, String, null: true
  end

  class VersionedPayloadUpdatedV2 < GraphQL::Schema::Subscription
    field :str, Integer, null: true, method: :str_int
  end

  class VersionedSubscriptionRootV1 < GraphQL::Schema::Object
    field :payload_updated, subscription: VersionedPayloadUpdatedV1
  end

  class VersionedSubscriptionRootV2 < GraphQL::Schema::Object
    field :payload_updated, subscription: VersionedPayloadUpdatedV2
  end

  class VersionedQueryRoot < GraphQL::Schema::Object
    field :int, Integer, null: true
  end

  def build_versioned_storage
    {
      subscriptions: {},
      events: Hash.new { |h, k| h[k] = [] },
      deliveries: Hash.new { |h, k| h[k] = [] },
      terminations: [],
    }
  end

  def build_versioned_schema(storage, version, subscription_root)
    Class.new(GraphQL::Schema) do
      query(VersionedQueryRoot)
      subscription(subscription_root)
      use(VersionedUpdateBackend, storage: storage, schema_version: version)
    end
  end

  SUBSCRIPTION_QUERY = "subscription { payloadUpdated { str } }"

  before do
    GraphQL::Subscriptions.reset_schema_versions!
  end

  after do
    GraphQL::Subscriptions.reset_schema_versions!
  end

  it "remembers the validating version in persisted data and routes updates to it instead of migrating same-named fields" do
    storage = build_versioned_storage
    v1_schema = build_versioned_schema(storage, "v1", VersionedSubscriptionRootV1)
    v2_schema = build_versioned_schema(storage, "v2", VersionedSubscriptionRootV2)

    res_v1 = v1_schema.execute(SUBSCRIPTION_QUERY)
    sub_id_v1 = res_v1.context[:subscription_id]
    res_v2 = v2_schema.execute(SUBSCRIPTION_QUERY)
    sub_id_v2 = res_v2.context[:subscription_id]

    # Persisted data remembers which version each subscription was validated with:
    assert_equal "v1", storage[:subscriptions][sub_id_v1][:schema_version]
    assert_equal "v2", storage[:subscriptions][sub_id_v2][:schema_version]

    # Trigger only on the new schema's instance; both subscriptions share a topic:
    v2_schema.subscriptions.trigger("payloadUpdated", {}, VersionedObject.new("v1-string", 222))

    # The old subscription was executed by its own version, not migrated to the new schema:
    v1_delivery = storage[:deliveries][sub_id_v1].last
    assert_equal "v1-string", v1_delivery.to_h.dig("data", "payloadUpdated", "str")
    assert_equal "v1", v1_delivery.context.namespace(:subscriptions)[:schema_version]

    # The new subscription used the new schema's same-named field with its own output type:
    v2_delivery = storage[:deliveries][sub_id_v2].last
    assert_equal 222, v2_delivery.to_h.dig("data", "payloadUpdated", "str")
    assert_equal "v2", v2_delivery.context.namespace(:subscriptions)[:schema_version]

    assert_empty storage[:terminations]
  end

  it "terminates subscriptions of a retired version with the version's reason while other versions keep updating" do
    storage = build_versioned_storage
    v1_schema = build_versioned_schema(storage, "v1", VersionedSubscriptionRootV1)
    v2_schema = build_versioned_schema(storage, "v2", VersionedSubscriptionRootV2)

    sub_id_v1 = v1_schema.execute(SUBSCRIPTION_QUERY).context[:subscription_id]
    sub_id_v2 = v2_schema.execute(SUBSCRIPTION_QUERY).context[:subscription_id]

    # Retire the old version, then trigger on the new schema:
    GraphQL::Subscriptions.retire_schema_version("v1", reason: "v1 was replaced by v2")
    v2_schema.subscriptions.trigger("payloadUpdated", {}, VersionedObject.new("v1-string", 222))

    # The old subscription was terminated, with its own version and reason:
    assert_equal 1, storage[:terminations].length
    termination = storage[:terminations].first
    assert_equal sub_id_v1, termination[:subscription_id]
    assert_equal "v1", termination[:version]
    assert_equal "v1 was replaced by v2", termination[:reason]
    assert_nil storage[:subscriptions][sub_id_v1]
    assert_empty storage[:deliveries][sub_id_v1]

    # The un-retired subscription keeps updating:
    v2_delivery = storage[:deliveries][sub_id_v2].last
    assert_equal 222, v2_delivery.to_h.dig("data", "payloadUpdated", "str")
    assert_equal "v2", v2_delivery.context.namespace(:subscriptions)[:schema_version]

    # Triggering again is a no-op for the terminated subscription:
    v2_schema.subscriptions.trigger("payloadUpdated", {}, VersionedObject.new("v1-string", 333))
    assert_equal 1, storage[:terminations].length
    assert_equal 2, storage[:deliveries][sub_id_v2].length
    assert_equal 333, storage[:deliveries][sub_id_v2].last.to_h.dig("data", "payloadUpdated", "str")
  end

  it "terminates subscriptions when their own retired version's instance receives an update" do
    storage = build_versioned_storage
    v1_schema = build_versioned_schema(storage, "v1", VersionedSubscriptionRootV1)
    _v2_schema = build_versioned_schema(storage, "v2", VersionedSubscriptionRootV2)

    sub_id_v1 = v1_schema.execute(SUBSCRIPTION_QUERY).context[:subscription_id]

    GraphQL::Subscriptions.retire_schema_version("v1")
    v1_schema.subscriptions.trigger("payloadUpdated", {}, VersionedObject.new("v1-string", 222))

    termination = storage[:terminations].first
    assert_equal sub_id_v1, termination[:subscription_id]
    assert_equal "v1", termination[:version]
    assert_equal "Schema version \"v1\" was retired", termination[:reason]
    assert_nil storage[:subscriptions][sub_id_v1]
    assert_empty storage[:deliveries][sub_id_v1]
  end

  it "keeps updating subscriptions whose persisted data has no version" do
    storage = build_versioned_storage
    v1_schema = build_versioned_schema(storage, "v1", VersionedSubscriptionRootV1)
    _v2_schema = build_versioned_schema(storage, "v2", VersionedSubscriptionRootV2)

    sub_id_v1 = v1_schema.execute(SUBSCRIPTION_QUERY).context[:subscription_id]
    # Simulate legacy persisted data from before versions were tracked:
    storage[:subscriptions][sub_id_v1].delete(:schema_version)

    GraphQL::Subscriptions.retire_schema_version("v2", reason: "retiring the other version")
    v1_schema.subscriptions.trigger("payloadUpdated", {}, VersionedObject.new("v1-string", 222))

    v1_delivery = storage[:deliveries][sub_id_v1].last
    assert_equal "v1-string", v1_delivery.to_h.dig("data", "payloadUpdated", "str")
    assert_empty storage[:terminations]
  end
end
