require 'test_helper'
require 'flipper/adapters/failover'
require 'flipper/adapters/instrumented'
require 'flipper/adapters/memoizable'

class MemoizableReceiptTest < MiniTest::Test
  FailingGet = Class.new(RuntimeError)

  def setup
    @primary_store = Flipper::Adapters::Memory.new
    @secondary_store = Flipper::Adapters::Memory.new
    @failover = Flipper::Adapters::Failover.new(
      @primary_store, @secondary_store, receipt: true
    )
    @memoizable = Flipper::Adapters::Memoizable.new(@failover)
    @memoizable.memoize = true
    @flipper = Flipper.new(@memoizable, memoize: false)
    @feature = @flipper[:stats]
  end

  def test_exposes_the_wrapped_failover_receipt
    assert_same @failover.read_receipt, @memoizable.read_receipt
  end

  def test_returns_nil_receipt_when_wrapped_adapter_has_none
    memoizable = Flipper::Adapters::Memoizable.new(Flipper::Adapters::Memory.new)
    assert_nil memoizable.read_receipt
  end

  def test_first_read_keeps_primary_provenance_and_memo_hit_is_restamped
    @memoizable.get(@feature)
    entry = @memoizable.read_receipt.for(@feature.key)
    assert_equal :primary, entry.source
    assert_equal :missing, entry.status

    @memoizable.get(@feature)
    entry = @memoizable.read_receipt.for(@feature.key)
    assert_equal :memo, entry.source
    assert_equal :primary, entry.active_source
    assert_equal :missing, entry.active_status
  end

  def test_degraded_value_cached_in_one_call_is_memo_on_the_next
    def @primary_store.get(feature)
      raise FailingGet, "primary down"
    end
    enable_boolean(@secondary_store)

    @memoizable.get(@feature)
    entry = @memoizable.read_receipt.for(@feature.key)
    assert_equal :secondary, entry.source
    assert_equal :degraded, entry.status

    @memoizable.get(@feature)
    entry = @memoizable.read_receipt.for(@feature.key)
    assert_equal :memo, entry.source
    assert_equal :secondary, entry.active_source
    assert_equal :degraded, entry.active_status
    assert_kind_of FailingGet, entry.error
  end

  def test_actively_reread_after_memo_restores_primary_provenance
    @memoizable.get(@feature)
    @memoizable.get(@feature)
    assert_equal :memo, @memoizable.read_receipt.for(@feature.key).source

    # Expire the memoized value as a write would, then actively read again.
    @memoizable.expire_feature_for_test(@feature)
    @memoizable.get(@feature)

    entry = @memoizable.read_receipt.for(@feature.key)
    assert_equal :primary, entry.source
    assert_nil entry.original_source
  end

  def test_get_multi_only_restamps_cached_keys
    other = @flipper[:search]
    @memoizable.get(@feature)

    result = @memoizable.get_multi([@feature, other])

    assert_equal @memoizable.default_config, result[@feature.key]
    assert_equal :memo, @memoizable.read_receipt.for(@feature.key).source
    assert_equal :primary, @memoizable.read_receipt.for(@feature.key).active_source

    other_entry = @memoizable.read_receipt.for(other.key)
    assert_equal :primary, other_entry.source
    assert_equal :missing, other_entry.status
    refute other_entry.memo?
  end

  def test_toggling_memoization_clears_cached_provenance
    @memoizable.get(@feature)
    @memoizable.get(@feature)
    assert_equal :memo, @memoizable.read_receipt.for(@feature.key).source

    @memoizable.memoize = false
    @memoizable.memoize = true
    @memoizable.get(@feature)

    assert_equal :primary, @memoizable.read_receipt.for(@feature.key).source
  end

  def test_preloaded_missing_default_is_stamped_from_the_memo
    @memoizable.get_all
    unknown = @flipper[:never_added]

    @memoizable.get(unknown)

    entry = @memoizable.read_receipt.for(unknown.key)
    assert_equal :memo, entry.source
    assert_equal :missing, entry.status
  end

  def test_receipt_stays_reachable_through_an_instrumented_wrapper
    instrumented = Flipper::Adapters::Instrumented.new(
      @failover, instrumenter: Flipper::Instrumenters::Memory.new
    )
    memoizable = Flipper::Adapters::Memoizable.new(instrumented)
    memoizable.memoize = true

    assert_same @failover.read_receipt, memoizable.read_receipt

    memoizable.get(@feature)
    memoizable.get(@feature)
    entry = @failover.read_receipt.for(@feature.key)
    assert_equal :memo, entry.source
    assert_equal :primary, entry.active_source
  end

  private

  def enable_boolean(store)
    store.add(@feature)
    store.enable(@feature, @feature.gate(:boolean), Flipper::Types::Boolean.new)
  end
end

# Internal: expose the private expiry helper for the reread test.
class Flipper::Adapters::Memoizable
  def expire_feature_for_test(feature)
    send(:expire_feature, feature)
  end
end
