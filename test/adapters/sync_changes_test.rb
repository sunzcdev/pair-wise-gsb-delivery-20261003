require 'test_helper'
require 'flipper/adapters/sync/change_set'
require 'flipper/adapters/sync/synchronizer'

class SyncChangesTest < MiniTest::Test
  ChangeSet = Flipper::Adapters::Sync::ChangeSet
  SyncVersion = Flipper::SyncVersion

  def setup
    @local = Flipper::Adapters::Memory.new
    @local_flipper = Flipper.new(@local)
    @remote = Flipper::Adapters::Memory.new
    @synchronizer = Flipper::Adapters::Sync::Synchronizer.new(@local, @remote, raise: true)
  end

  def version(number)
    SyncVersion.new("scope", number)
  end

  def test_increment_keeps_the_values_it_does_not_mention
    @local_flipper.enable(:search)
    @local_flipper[:search].enable_group(:staff)
    @local_flipper[:search].enable_percentage_of_time(25)

    @synchronizer.apply(ChangeSet.incremental({search: {actors: Set["1"]}}))

    values = @local_flipper[:search].gate_values
    assert_equal Set["1"], values.actors
    assert_equal Set["staff"], values.groups
    assert_equal 25, values.percentage_of_time
    assert_equal true, values.boolean
  end

  def test_increment_only_clears_the_gates_it_names
    @local_flipper.enable(:search)
    @local_flipper[:search].enable_group(:staff)
    @local_flipper[:search].enable_percentage_of_time(25)

    @synchronizer.apply(ChangeSet.incremental({search: {removed_gates: [:groups]}}))

    values = @local_flipper[:search].gate_values
    assert_equal Set.new, values.groups
    assert_equal 25, values.percentage_of_time
  end

  def test_increment_removes_a_feature_only_when_it_says_so
    @local_flipper.enable(:search)
    @local_flipper.enable(:stats)

    @synchronizer.apply(ChangeSet.incremental({search: {deleted: true}}))

    assert_equal ["stats"], @local.features.to_a
  end

  def test_complete_mirror_still_overwrites
    @local_flipper.enable(:search)
    @local_flipper[:search].enable_group(:staff)
    @local_flipper.enable(:stats)

    @synchronizer.apply(ChangeSet.full({search: {boolean: true}}))

    assert_equal ["search"], @local.features.to_a
    assert_equal Set.new, @local_flipper[:search].gate_values.groups
  end

  def test_every_change_of_one_update_settles_at_its_version
    @local_flipper.enable(:a)
    @local_flipper.enable(:b)
    @local_flipper.enable(:c)
    @local.sync_version = version(1)

    @synchronizer.apply(ChangeSet.incremental(
      {a: {boolean: false}, b: {deleted: true}, d: {boolean: true}},
      version: version(2),
    ))

    assert_equal ["a", "c", "d"], @local.features.to_a.sort
    assert_equal false, @local_flipper[:a].boolean_value
    assert_equal version(2), @local.sync_version
  end

  def test_a_replayed_older_increment_does_not_resurrect_a_removed_gate
    @local_flipper.enable(:a)
    @local.sync_version = version(5)

    @synchronizer.apply(ChangeSet.incremental({a: {boolean: true}}, version: version(4)))

    assert_equal true, @local_flipper[:a].boolean_value
    assert_equal version(5), @local.sync_version
  end

  def test_a_failing_update_leaves_the_previous_version_readable
    @local_flipper.enable(:a)
    @local.sync_version = version(5)
    before = @local.get_all

    exploding = Flipper::Adapters::Memory.new
    exploding.define_singleton_method(:enable) do |*|
      raise IOError, "stubbed failure"
    end
    failing = Flipper::Adapters::Sync::Synchronizer.new(exploding, @remote, raise: true)

    assert_raises(IOError) do
      failing.apply(ChangeSet.incremental(
        {a: {removed_gates: [:boolean]}, b: {deleted: true}, d: {boolean: true}},
        version: version(6),
      ))
    end

    assert_equal before, @local.get_all
    assert_equal version(5), @local.sync_version
    assert_equal ["a"], @local.features.to_a
  end
end