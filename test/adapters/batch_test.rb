require 'test_helper'
require 'flipper/adapters/batch'

class Flipper::Adapters::BatchTest < MiniTest::Test
  prepend Flipper::Test::SharedAdapterTests

  def setup
    @source = Flipper::Adapters::Memory.new
    @adapter = Flipper::Adapters::Batch.from(@source)
    @flipper = Flipper.new(@adapter)
    @feature = @flipper[:stats]
    @boolean_gate = @feature.gate(:boolean)
    @group_gate = @feature.gate(:group)
    @expression_gate = @feature.gate(:expression)
    @actor_gate = @feature.gate(:actor)
    @actors_gate = @feature.gate(:percentage_of_actors)
    @time_gate = @feature.gate(:percentage_of_time)
  end

  def test_starts_empty_without_a_source
    batch = Flipper::Adapters::Batch.new
    assert_equal Set.new, batch.features
    assert_equal({}, batch.get_all)
    assert_empty batch.changes
  end

  def test_seeds_with_committed_state_of_source
    source_flipper = Flipper.new(@source)
    source_flipper.enable(:stats)
    source_flipper.enable_actor(:search, Flipper::Actor.new('22'))

    batch = Flipper::Adapters::Batch.from(@source)

    assert_equal 'true', batch.get(source_flipper[:stats])[:boolean]
    assert_equal Set['22'], batch.get(source_flipper[:search])[:actors]
    assert_equal Set['stats', 'search'], batch.features
  end

  def test_writes_do_not_mutate_the_source
    source_flipper = Flipper.new(@source)
    source_flipper.enable(:stats)
    feature = source_flipper[:search]

    @adapter.add(feature)
    @adapter.enable(feature, feature.gate(:boolean), Flipper::Types::Boolean.new(true))

    assert_equal Set['stats'], @source.features
    assert_equal @source.default_config, @source.get(feature)
    assert_includes @adapter.features, 'search'
  end

  def test_tracks_changed_features
    feature = @flipper[:stats]
    refute @adapter.changed?(feature)

    @adapter.add(feature)

    assert @adapter.changed?(feature)
    assert_equal Set['stats'], @adapter.changes
  end

  def test_get_all_is_importable
    source_flipper = Flipper.new(@source)
    source_flipper.add(:stats)
    source_flipper.enable_actor(:stats, Flipper::Actor.new('22'))
    batch = Flipper::Adapters::Batch.from(@source)
    feature = source_flipper[:stats]
    # Enabling the boolean gate resets the other gates (matching Memory),
    # then re-add the actor so both end up in the committed snapshot.
    batch.enable(feature, feature.gate(:boolean), Flipper::Types::Boolean.new(true))
    batch.enable(feature, feature.gate(:actor), Flipper::Types::Actor.new(Flipper::Actor.new('22')))

    destination = Flipper::Adapters::Memory.new
    destination.import(batch)

    assert_equal 'true', destination.get(feature)[:boolean]
    assert_equal Set['22'], destination.get(feature)[:actors]
  end

  def test_committing_shares_no_mutable_state_with_the_batch
    source_flipper = Flipper.new(@source)
    source_flipper.add(:stats)
    source_flipper.enable_actor(:stats, Flipper::Actor.new('22'))
    batch = Flipper::Adapters::Batch.from(@source)
    feature = source_flipper[:stats]

    destination = Flipper::Adapters::Memory.new
    destination.import(batch)

    batch.get(feature)[:actors] << '99'

    assert_equal Set['22'], destination.get(feature)[:actors]
  end

  def test_buffered_set_writes_do_not_leak_into_the_source
    source_flipper = Flipper.new(@source)
    source_flipper.add(:stats)
    source_flipper.enable_actor(:stats, Flipper::Actor.new('22'))

    batch = Flipper::Adapters::Batch.from(@source)
    feature = source_flipper[:stats]
    batch.enable(feature, feature.gate(:actor), Flipper::Types::Actor.new(Flipper::Actor.new('99')))

    assert_equal Set['22'], @source.get(feature)[:actors]
    assert_equal Set['22', '99'], batch.get(feature)[:actors]
  end
end
