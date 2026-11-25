require 'test_helper'
require 'flipper/adapters/failover'

# Internal: A primary adapter whose raises can be scripted per operation while
# the actual gate values live in an in-memory adapter.
class ScriptedPrimaryAdapter
  def initialize(store)
    @store = store
    @raised = Hash.new { |hash, key| hash[key] = [] }
  end

  attr_reader :raised

  # Public: Raise +error+ on the next +count+ calls of +operation+.
  def fail(operation, error, count: 1)
    @raised[operation].concat(Array.new(count) { error })
  end

  def get(feature)
    maybe_raise(:get)
    @store.get(feature)
  end

  def get_multi(features)
    maybe_raise(:get_multi)
    @store.get_multi(features)
  end

  def get_all(**kwargs)
    maybe_raise(:get_all)
    @store.get_all(**kwargs)
  end

  def features
    maybe_raise(:features)
    @store.features
  end

  def add(feature); @store.add(feature); end
  def remove(feature); @store.remove(feature); end
  def clear(feature); @store.clear(feature); end
  def enable(feature, gate, thing); @store.enable(feature, gate, thing); end
  def disable(feature, gate, thing); @store.disable(feature, gate, thing); end
  def read_only?; false; end
  def sync_version; @store.sync_version; end
  def sync_version=(version); @store.sync_version = version; end
  def adapter_stack; "scripted_primary -> #{@store.adapter_stack}"; end
  def name; :scripted_primary; end

  private

  def maybe_raise(operation)
    error = @raised[operation].shift
    raise error if error
  end
end

class FailoverReceiptTest < MiniTest::Test
  def setup
    @primary_store = Flipper::Adapters::Memory.new
    @secondary_store = Flipper::Adapters::Memory.new
    @primary = ScriptedPrimaryAdapter.new(@primary_store)
    @adapter = Flipper::Adapters::Failover.new(@primary, @secondary_store, receipt: true)
    @flipper = Flipper.new(@adapter)
    @healthy = @flipper[:healthy]
    @flaky = @flipper[:flaky]
  end

  def test_collects_no_receipt_when_disabled
    adapter = Flipper::Adapters::Failover.new(
      Flipper::Adapters::Memory.new,
      Flipper::Adapters::Memory.new,
    )
    assert_nil adapter.read_receipt
  end

  def test_healthy_primary_explicit_missing_is_not_filled_with_stale_secondary
    enable_boolean(@secondary_store, @flaky)

    value = @adapter.get(@flaky)

    assert_equal @adapter.default_config, value
    entry = @adapter.read_receipt.for(@flaky.key)
    assert_equal :primary, entry.source
    assert_equal :missing, entry.status
    assert_nil entry.value[:boolean]
    assert_nil entry.error
    assert entry.missing?
    assert entry.primary?
  end

  def test_healthy_primary_stored_value_is_recorded_as_ok
    enable_boolean(@primary_store, @flaky)

    entry = @adapter.read_receipt.tap { @adapter.get(@flaky) }.for(@flaky.key)

    assert_equal :primary, entry.source
    assert_equal :ok, entry.status
    assert_equal "true", entry.value[:boolean]
  end

  def test_batch_failure_returns_secondary_values_and_records_the_failure_point
    enable_boolean(@secondary_store, @flaky)
    error = RuntimeError.new("primary unavailable")
    @primary.fail(:get_multi, error)

    result = @adapter.get_multi([@healthy, @flaky])

    assert_nil result[@healthy.key][:boolean]
    assert_equal "true", result[@flaky.key][:boolean]

    receipt = @adapter.read_receipt
    [@healthy, @flaky].each do |feature|
      entry = receipt.for(feature.key)
      assert_equal :secondary, entry.source
      assert_equal :degraded, entry.status
      assert_same error, entry.error
      assert_equal :get_multi, entry.operation
    end

    failure = receipt.failures.last
    assert_equal :get_multi, failure.operation
    assert_equal [@healthy.key, @flaky.key], failure.keys
    assert_same error, failure.error
  end

  def test_get_all_failure_marks_every_returned_key_degraded
    enable_boolean(@secondary_store, @flaky)
    error = RuntimeError.new("primary down")
    @primary.fail(:get_all, error)

    result = @adapter.get_all

    assert_includes result.keys, @flaky.key
    @adapter.read_receipt.entries.each_value do |entry|
      assert_equal :secondary, entry.source
      assert_equal :degraded, entry.status
      assert_same error, entry.error
    end
    assert_equal :get_all, @adapter.read_receipt.failures.last.operation
  end

  def test_features_failure_is_recorded_on_the_reserved_entry
    error = RuntimeError.new("features down")
    @primary.fail(:features, error)

    @adapter.features

    entry = @adapter.read_receipt.features_entry
    assert_equal :secondary, entry.source
    assert_equal :degraded, entry.status
    assert_same error, entry.error
    assert_equal :features, entry.operation
  end

  def test_adapter_contract_return_values_are_unchanged_under_failure
    enable_boolean(@secondary_store, @flaky)
    @primary.fail(:get, RuntimeError.new("down"))

    # A plain failover adapter (no receipt) and the receipt adapter must
    # return identical readable values.
    plain = Flipper::Adapters::Failover.new(@primary, @secondary_store)
    @primary.fail(:get, RuntimeError.new("down"))

    assert_equal plain.get(@flaky), @adapter.get(@flaky)
  end

  def test_full_recovery_narrative_with_interleaved_memoization
    # The secondary always holds an enabled (stale) flaky flag.
    enable_boolean(@secondary_store, @flaky)

    receipt = @adapter.read_receipt

    # Request 1: healthy primary explicitly reports flaky as missing. The
    # stale secondary enabled value must not be served.
    @flipper.memoize = true
    refute @flipper[:flaky].enabled?
    flaky_entry = receipt.for(@flaky.key)
    assert_equal :primary, flaky_entry.source
    assert_equal :missing, flaky_entry.status
    assert_nil flaky_entry.value[:boolean]

    # Interleave a healthy key inside the same request cache.
    refute @flipper[:healthy].enabled?
    healthy_entry = receipt.for(@healthy.key)
    assert_equal :primary, healthy_entry.source
    assert_equal :missing, healthy_entry.status

    # A second read in the same request is served from the memo and keeps
    # the active provenance alongside the :memo source.
    @flipper[:healthy].enabled?
    healthy_entry = receipt.for(@healthy.key)
    assert_equal :memo, healthy_entry.source
    assert_equal :primary, healthy_entry.active_source
    @flipper.memoize = false

    # Request 2: the primary fails in the middle of a batch read; the whole
    # batch degrades to the secondary.
    batch_error = RuntimeError.new("primary failed mid batch")
    @primary.fail(:get_multi, batch_error)
    @flipper.memoize = true

    batch = @flipper.adapter.get_multi([@healthy, @flaky])
    assert_nil batch[@healthy.key][:boolean]
    assert_equal "true", batch[@flaky.key][:boolean]
    [@healthy, @flaky].each do |feature|
      entry = receipt.for(feature.key)
      assert_equal :secondary, entry.source
      assert_equal :degraded, entry.status
      assert_same batch_error, entry.error
    end

    # enabled? calls inside the same request reuse the memoized degraded
    # values: flaky looks enabled even though the primary never said so.
    assert @flipper[:flaky].enabled?
    flaky_entry = receipt.for(@flaky.key)
    assert_equal :memo, flaky_entry.source
    assert_equal :secondary, flaky_entry.active_source
    assert_equal :degraded, flaky_entry.active_status

    # Interleave the healthy key again; it is also a memo hit carrying the
    # degraded active provenance.
    refute @flipper[:healthy].enabled?
    healthy_entry = receipt.for(@healthy.key)
    assert_equal :memo, healthy_entry.source
    assert_equal :secondary, healthy_entry.active_source
    @flipper.memoize = false

    # Request 3: the primary recovers and now returns an explicit disabled
    # flaky value.
    @primary_store.add(@flaky)
    @primary_store.disable(@flaky, @flaky.gate(:boolean),
                           Flipper::Types::Boolean.new(false))
    @flipper.memoize = true

    refute @flipper[:flaky].enabled?
    flaky_entry = receipt.for(@flaky.key)
    assert_equal :primary, flaky_entry.source
    assert_equal :missing, flaky_entry.status
    assert_nil flaky_entry.value[:boolean]
    assert_nil flaky_entry.original_source

    # The healthy key is actively read from the recovered primary too.
    refute @flipper[:healthy].enabled?
    healthy_entry = receipt.for(@healthy.key)
    assert_equal :primary, healthy_entry.source
    assert_equal :missing, healthy_entry.status
    @flipper.memoize = false

    # The receipt still audits every failure point after recovery.
    operations = receipt.failures.map(&:operation)
    assert_includes operations, :get_multi
    batch_failure = receipt.failures.find { |failure| failure.operation == :get_multi }
    assert_equal [@healthy.key, @flaky.key], batch_failure.keys
    assert_same batch_error, batch_failure.error
  end

  private

  def enable_boolean(store, feature)
    store.add(feature)
    store.enable(feature, feature.gate(:boolean), Flipper::Types::Boolean.new)
  end
end
