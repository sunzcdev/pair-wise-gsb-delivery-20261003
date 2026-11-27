require 'test_helper'
require 'flipper/adapters/dual_write'

class Flipper::Adapters::DualWriteTransactionTest < MiniTest::Test
  OLD_KEY = 'requests_use_old_backend'.freeze
  NEW_KEY = 'requests_use_new_backend'.freeze

  def setup
    @local = Flipper::Adapters::Memory.new
    @remote = Flipper::Adapters::Memory.new
    @adapter = Flipper::Adapters::DualWrite.new(@local, @remote)
    @flipper = Flipper.new(@adapter)
    @old_backend = @flipper[OLD_KEY]
    @new_backend = @flipper[NEW_KEY]
  end

  # Only one backend may serve requests: the legal combinations are
  # [old on, new off] and [old off, new on]. Anything else is rejected.
  def validate_exactly_one_backend
    lambda do |batch|
      old_on, new_on = boolean_pair(batch)
      next if old_on != new_on

      raise Flipper::InvalidBatch,
            "exactly one backend must serve requests, got old=#{old_on} new=#{new_on}"
    end
  end

  def boolean_pair(adapter)
    all = adapter.get_all
    [
      all[OLD_KEY]&.fetch(:boolean, nil) == 'true',
      all[NEW_KEY]&.fetch(:boolean, nil) == 'true',
    ]
  end

  def enable_boolean(feature)
    @adapter.enable(feature, feature.gate(:boolean), Flipper::Types::Boolean.new(true))
  end

  def disable_boolean(feature)
    @adapter.disable(feature, feature.gate(:boolean), Flipper::Types::Boolean.new(false))
  end

  def test_normal_cutover_flips_both_flags_together_on_both_backends
    enable_boolean(@old_backend)
    disable_boolean(@new_backend)
    assert_equal [true, false], boolean_pair(@local)

    @adapter.transaction(validate: validate_exactly_one_backend) do |batch|
      batch.disable(@old_backend, @old_backend.gate(:boolean), Flipper::Types::Boolean.new(false))
      batch.enable(@new_backend, @new_backend.gate(:boolean), Flipper::Types::Boolean.new(true))
    end

    assert_equal [false, true], boolean_pair(@local)
    assert_equal [false, true], boolean_pair(@remote)
  end

  def test_read_barrier_between_the_two_modifications_only_shows_legal_states
    enable_boolean(@old_backend)
    disable_boolean(@new_backend)
    observed = []

    @adapter.transaction(validate: validate_exactly_one_backend) do |batch|
      batch.disable(@old_backend, @old_backend.gate(:boolean), Flipper::Types::Boolean.new(false))

      # Read barrier: between the two buffered modifications the original entry
      # still serves the previously committed (legal) state.
      observed << boolean_pair(@adapter)

      batch.enable(@new_backend, @new_backend.gate(:boolean), Flipper::Types::Boolean.new(true))
    end

    assert_equal [[true, false]], observed
    assert_equal [false, true], boolean_pair(@adapter)
  end

  def test_original_entry_keeps_reading_committed_state_inside_the_batch
    enable_boolean(@old_backend)

    @adapter.transaction do |batch|
      batch.enable(@new_backend, @new_backend.gate(:boolean), Flipper::Types::Boolean.new(true))

      assert_nil @adapter.get(@new_backend)[:boolean]
      assert_equal 'true', batch.get(@new_backend)[:boolean]
    end
  end

  def test_illegal_two_backend_batch_does_not_change_the_existing_gating
    disable_boolean(@old_backend)
    enable_boolean(@new_backend)
    before_local = boolean_pair(@local)
    before_remote = boolean_pair(@remote)

    error = assert_raises(Flipper::InvalidBatch) do
      @adapter.transaction(validate: validate_exactly_one_backend) do |batch|
        # new backend already on in committed state; enabling the old one too
        # would route requests to both backends at once.
        batch.enable(@old_backend, @old_backend.gate(:boolean), Flipper::Types::Boolean.new(true))
      end
    end

    assert_match(/exactly one backend/, error.message)
    assert_equal before_local, boolean_pair(@local)
    assert_equal before_remote, boolean_pair(@remote)
  end

  def test_illegal_zero_backend_batch_does_not_change_the_existing_gating
    disable_boolean(@old_backend)
    enable_boolean(@new_backend)

    assert_raises(Flipper::InvalidBatch) do
      @adapter.transaction(validate: validate_exactly_one_backend) do |batch|
        batch.disable(@new_backend, @new_backend.gate(:boolean), Flipper::Types::Boolean.new(false))
      end
    end

    assert_equal [false, true], boolean_pair(@local)
    assert_equal [false, true], boolean_pair(@remote)
  end

  def test_commits_without_a_validator_when_none_is_given
    @adapter.transaction do |batch|
      batch.enable(@new_backend, @new_backend.gate(:boolean), Flipper::Types::Boolean.new(true))
    end

    assert_equal [false, true], boolean_pair(@local)
    assert_equal [false, true], boolean_pair(@remote)
  end

  def test_returns_the_block_value
    result = @adapter.transaction { :cutover_complete }
    assert_equal :cutover_complete, result
  end
end
