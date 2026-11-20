# frozen_string_literal: true

require_relative "helper"

require "puma/tempfile_quota"

class TestTempfileQuota < PumaTest
  parallelize_me!

  def test_unlimited_by_default
    quota = Puma::TempfileQuota.new

    assert_nil quota.limit
    assert quota.reserve(1_000_000, :client)
    assert_equal 1_000_000, quota.used
  end

  def test_reserve_up_to_limit
    quota = Puma::TempfileQuota.new 100

    r1 = quota.reserve(60, :client_a)
    assert r1
    assert_equal 60, quota.used

    r2 = quota.reserve(40, :client_b)
    assert r2
    assert_equal 100, quota.used

    # aggregate quota is exhausted
    assert_nil quota.reserve(1, :client_c)
    assert_equal 100, quota.used
  end

  def test_release_allows_reuse
    quota = Puma::TempfileQuota.new 100

    r1 = quota.reserve(100, :client_a)
    assert_nil quota.reserve(1, :client_b)

    quota.release(r1)
    assert_equal 0, quota.used

    assert quota.reserve(100, :client_b)
  end

  def test_release_is_idempotent_and_ignores_unknown
    quota = Puma::TempfileQuota.new 100

    r1 = quota.reserve(50, :client_a)
    quota.release(r1)
    quota.release(r1)
    quota.release(nil)

    assert_equal 0, quota.used
    assert_empty quota.holders
  end

  def test_charge_grows_reservation_within_limit
    quota = Puma::TempfileQuota.new 100

    r1 = quota.reserve(0, :client_a)
    assert quota.charge(r1, 70)
    assert_equal 70, quota.used

    refute quota.charge(r1, 31)
    assert_equal 70, quota.used

    assert quota.charge(r1, 30)
    assert_equal 100, quota.used
    assert_equal 100, r1.bytes
  end

  def test_holders_track_owner_and_handoff
    quota = Puma::TempfileQuota.new 100

    r1 = quota.reserve(10, :client_a)
    r2 = quota.reserve(20, :client_b)

    # before handoff the client holds its own reservation
    assert_equal [[:client_a, :client_a, 10], [:client_b, :client_b, 20]],
      quota.holders.sort_by(&:last)

    # r1 is handed off to the Rack app via the env
    env = { 'rack.input' => :tempfile }
    quota.hand_off(r1, env)

    holder = quota.holders.find { |h| h.first == :client_a }
    assert_same env, holder[1]

    quota.release(r1)
    assert_equal [[:client_b, :client_b, 20]], quota.holders

    quota.release(r2)
    assert_empty quota.holders
    assert_equal 0, quota.used
  end
end
