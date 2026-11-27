require 'test_helper'
require 'flipper/adapters/pstore'

class Flipper::Adapters::TransactionTest < MiniTest::Test
  # Exercises the generic Adapter#transaction implementation on more than one
  # storage adapter (Memory overrides it to hold its lock; PStore uses the base
  # implementation).
  def build_adapters
    memory = Flipper::Adapters::Memory.new
    dir = FlipperRoot.join('tmp').tap(&:mkpath)
    path = dir.join("transaction-#{object_id}.pstore")
    path.unlink if path.exist?
    pstore = Flipper::Adapters::PStore.new(path)
    [memory, pstore]
  end

  def test_commits_a_group_of_buffered_changes_together
    build_adapters.each do |adapter|
      flipper = Flipper.new(adapter)
      feature = flipper[:stats]

      adapter.transaction do |batch|
        batch.add(feature)
        batch.enable(feature, feature.gate(:boolean), Flipper::Types::Boolean.new(true))
      end

      assert_equal Set['stats'], adapter.features
      assert_equal 'true', adapter.get(feature)[:boolean]
    end
  end

  def test_buffered_writes_are_invisible_through_the_original_entry_until_commit
    build_adapters.each do |adapter|
      flipper = Flipper.new(adapter)
      feature = flipper[:stats]

      adapter.transaction do |batch|
        batch.enable(feature, feature.gate(:boolean), Flipper::Types::Boolean.new(true))

        assert_equal adapter.default_config, adapter.get(feature)
        assert_equal 'true', batch.get(feature)[:boolean]
      end

      assert_equal 'true', adapter.get(feature)[:boolean]
    end
  end

  def test_legal_validated_batch_commits
    adapter = Flipper::Adapters::Memory.new
    flipper = Flipper.new(adapter)
    feature = flipper[:stats]
    validator = ->(batch) { raise Flipper::InvalidBatch unless batch.get(feature)[:boolean] == 'true' }

    adapter.transaction(validate: validator) do |batch|
      batch.enable(feature, feature.gate(:boolean), Flipper::Types::Boolean.new(true))
    end

    assert_equal 'true', adapter.get(feature)[:boolean]
  end

  def test_invalid_batch_is_discarded_and_leaves_state_untouched
    adapter = Flipper::Adapters::Memory.new
    flipper = Flipper.new(adapter)
    stats = flipper[:stats]
    search = flipper[:search]
    validator = lambda do |batch|
      if batch.get(stats)[:boolean] == 'true' && batch.get(search)[:boolean] == 'true'
        raise Flipper::InvalidBatch, 'stats and search cannot both be on'
      end
    end

    error = assert_raises(Flipper::InvalidBatch) do
      adapter.transaction(validate: validator) do |batch|
        batch.enable(stats, stats.gate(:boolean), Flipper::Types::Boolean.new(true))
        batch.enable(search, search.gate(:boolean), Flipper::Types::Boolean.new(true))
      end
    end

    assert_equal({}, adapter.get_all)
    assert_match(/cannot both be on/, error.message)
  end

  def test_returns_the_block_value
    adapter = Flipper::Adapters::Memory.new
    assert_equal :done, adapter.transaction { :done }
  end

  def test_concurrent_reader_never_sees_buffered_writes
    adapter = Flipper::Adapters::Memory.new
    flipper = Flipper.new(adapter)
    feature = flipper[:stats]
    inside_batch = Queue.new
    release_batch = Queue.new
    reads = Queue.new

    writer = Thread.new do
      adapter.transaction do |batch|
        batch.enable(feature, feature.gate(:boolean), Flipper::Types::Boolean.new(true))
        inside_batch << true
        release_batch.pop
      end
    end

    inside_batch.pop
    reader = Thread.new { reads << adapter.get(feature)[:boolean] }
    assert_equal 0, reads.size
    release_batch << true
    writer.join
    reader.join

    assert_equal 'true', reads.pop
  end
end
