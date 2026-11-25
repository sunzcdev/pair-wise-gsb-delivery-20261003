require 'test_helper'
require 'flipper/adapters/read_receipt'

class ReadReceiptTest < MiniTest::Test
  def setup
    @receipt = Flipper::Adapters::ReadReceipt.new
  end

  def test_starts_with_no_entries_or_failures
    assert_empty @receipt.entries
    assert_empty @receipt.failures
    assert_nil @receipt.for(:stats)
  end

  def test_records_provenance_for_each_key
    @receipt.record(:stats, source: :primary, status: :ok, operation: :get,
                    values: { "stats" => { boolean: "true" } })

    entry = @receipt.for(:stats)
    assert_equal "stats", entry.key
    assert_equal :primary, entry.source
    assert_equal :ok, entry.status
    assert_equal :get, entry.operation
    assert_equal({ boolean: "true" }, entry.value)
    assert_nil entry.error
    assert entry.primary?
    refute entry.memo?
  end

  def test_records_degraded_secondary_reads_with_the_error
    error = RuntimeError.new("primary down")
    @receipt.record([:stats, :search], source: :secondary, status: :degraded,
                    operation: :get_multi,
                    values: { "stats" => {}, "search" => {} }, error: error)

    assert_equal :secondary, @receipt.for(:stats).source
    assert_equal :degraded, @receipt.for(:search).status
    assert_same error, @receipt.for(:stats).error
    assert @receipt.for(:stats).degraded?
  end

  def test_entries_excludes_reserved_keys
    @receipt.record(:stats, source: :primary, status: :ok, operation: :get, values: { "stats" => {} })
    @receipt.record(Flipper::Adapters::ReadReceipt.features_key,
                    source: :primary, status: :ok, operation: :features)

    assert_equal ["stats"], @receipt.entries.keys
    refute_nil @receipt.features_entry
  end

  def test_failures_keep_every_failure_point
    error_one = RuntimeError.new("one")
    error_two = RuntimeError.new("two")
    @receipt.record_failure(:get, [:stats], error_one)
    @receipt.record_failure(:get_multi, [:stats, :search], error_two)

    assert_equal [:get, :get_multi], @receipt.failures.map(&:operation)
    assert_equal [%w(stats), %w(stats search)], @receipt.failures.map(&:keys)
    assert_same error_one, @receipt.failures.first.error
  end

  def test_restamp_preserves_active_provenance_and_restore_puts_it_back
    error = RuntimeError.new("down")
    @receipt.record(:stats, source: :secondary, status: :degraded,
                    operation: :get_multi, values: { "stats" => {} }, error: error)

    @receipt.restamp(:stats, :get)
    entry = @receipt.for(:stats)
    assert_equal :memo, entry.source
    assert_equal :get, entry.operation
    assert_equal :secondary, entry.active_source
    assert_equal :degraded, entry.active_status
    assert entry.memo?

    @receipt.restore(:stats)
    entry = @receipt.for(:stats)
    assert_equal :secondary, entry.source
    assert_equal :degraded, entry.status
    assert_nil entry.original_source
    assert_nil entry.original_status
  end

  def test_restamp_is_a_noop_for_unknown_keys
    @receipt.restamp(:never_read, :get)
    assert_nil @receipt.for(:never_read)
  end

  def test_record_replaces_the_most_recent_entry
    @receipt.record(:stats, source: :primary, status: :missing,
                    operation: :get, values: { "stats" => {} })
    @receipt.record(:stats, source: :primary, status: :ok,
                    operation: :get, values: { "stats" => { boolean: "true" } })

    entry = @receipt.for(:stats)
    assert_equal :ok, entry.status
    assert_equal({ boolean: "true" }, entry.value)
  end

  def test_clear_empties_entries_and_failures
    @receipt.record(:stats, source: :primary, status: :ok,
                    operation: :get, values: { "stats" => {} })
    @receipt.record_failure(:get, [:stats], RuntimeError.new)

    @receipt.clear

    assert_empty @receipt.entries
    assert_empty @receipt.failures
  end
end
