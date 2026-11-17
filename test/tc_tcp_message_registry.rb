# frozen_string_literal: true

require_relative 'spec_helper'

# Unit coverage for the explicit 16-bit message ID reservation boundary used by
# a multiplexed TCP connection. The registry enforces unique outstanding IDs,
# question-aware matching, connection generation identity and explicit release
# semantics without performing any I/O.
class TestTcpMessageRegistry < Minitest::Test
  def question(name = 'reserved.example.')
    Dnsruby::Message.new(name).question
  end

  def build(capacity: Dnsruby::TcpMessageRegistry::CAPACITY, generation: 7)
    Dnsruby::TcpMessageRegistry.new(generation: generation, capacity: capacity)
  end

  def test_generation_and_capacity_are_validated
    assert_raises(ArgumentError) { Dnsruby::TcpMessageRegistry.new(generation: 0) }
    assert_raises(ArgumentError) { Dnsruby::TcpMessageRegistry.new(generation: 1, capacity: 0) }
    assert_raises(ArgumentError) do
      Dnsruby::TcpMessageRegistry.new(generation: 1,
        capacity: Dnsruby::TcpMessageRegistry::CAPACITY + 1)
    end
  end

  def test_reserve_returns_monotonic_serials_and_tracks_ownership
    registry = build
    first = registry.reserve(100, :a, question)
    second = registry.reserve(101, :b, question('other.example.'))
    assert_equal 1, first
    assert_equal 2, second
    assert registry.reserved?(100)
    assert_equal 2, registry.size
    refute registry.empty?
    assert_equal 7, registry.entry_for(100).generation
    assert_equal :a, registry.entry_for(100).client_query_id
  end

  def test_header_id_is_validated
    registry = build
    assert_raises(ArgumentError) { registry.reserve(-1, :a, question) }
    assert_raises(ArgumentError) { registry.reserve(65_536, :a, question) }
  end

  def test_duplicate_reservation_collides_and_never_overwrites
    registry = build
    registry.reserve(4242, :original, question('first.example.'))
    result = registry.reserve(4242, :intruder, question('second.example.'))
    assert_equal Dnsruby::TcpMessageRegistry::ID_COLLISION, result
    entry = registry.entry_for(4242)
    assert_equal :original, entry.client_query_id
    assert_equal 'first.example', entry.question_signature.first.qname.to_s
  end

  def test_saturation_is_reported_instead_of_overwriting
    registry = build(capacity: 3)
    serials = [10, 11, 12].map { |id| registry.reserve(id, "q#{id}".to_sym, question) }
    assert_equal [1, 2, 3], serials
    assert_equal Dnsruby::TcpMessageRegistry::SATURATED, registry.reserve(13, :extra, question)
    assert_equal 3, registry.size
  end

  def test_explicit_release_frees_an_id_and_requires_the_owner
    registry = build
    registry.reserve(500, :owner, question)
    refute registry.release(500, :someone_else)
    assert registry.reserved?(500)
    released = registry.release(500, :owner, answered: true)
    assert_equal 500, released.header_id
    refute registry.reserved?(500)
    assert released.answered?
    # Releasing again, or an unknown ID, is a no-op rather than an error.
    assert_nil registry.release(500, :owner)
    assert_nil registry.release(999, :owner)
  end

  def test_released_id_can_be_reserved_again_but_keeps_a_new_serial
    registry = build
    assert_equal 1, registry.reserve(7, :first, question)
    registry.release(7, :first, answered: true)
    assert_equal 2, registry.reserve(7, :second, question)
    assert_equal :second, registry.entry_for(7).client_query_id
  end

  def test_match_requires_generation_question_and_live_reservation
    registry = build(generation: 11)
    registry.reserve(900, :q, question('match.example.'))
    response = Dnsruby::Message.new('match.example.')
    response.header.id = 900
    entry = registry.match(900, response.question)
    assert_equal :q, entry.client_query_id
    assert_equal 11, entry.generation

    wrong_question = Dnsruby::Message.new('different.example.')
    wrong_question.header.id = 900
    assert_nil registry.match(900, wrong_question.question)
    assert_nil registry.match(901, response.question)
  end

  def test_questionless_tcp_error_reply_matches_its_reservation
    # A nil signature marks a raw/update transaction whose question section the
    # caller does not want compared; the ID and generation still identify it.
    unsigned = build
    unsigned.reserve(333, :raw, nil)
    assert unsigned.match(333, [], allow_questionless_error: true)
    assert unsigned.match(333, [], allow_questionless_error: false)

    signed = build
    signed.reserve(334, :normal, question)
    assert signed.match(334, [], allow_questionless_error: true)
    refute signed.match(334, [], allow_questionless_error: false)
  end

  def test_released_reservation_no_longer_matches_a_late_response
    registry = build
    registry.reserve(808, :late, question('late.example.'))
    registry.release(808, :late)
    response = Dnsruby::Message.new('late.example.')
    response.header.id = 808
    assert_nil registry.match(808, response.question)
  end

  def test_raw_and_update_reservations_match_on_id_without_question_comparison
    registry = build
    update = Dnsruby::Update.new('zone.example.')
    registry.reserve(424, :update,
      Dnsruby::TcpMessageRegistry.question_signature(update.question), question_optional: true)
    # A reply carrying a different question section still matches the raw/update
    # transaction on ID and generation, mirroring the legacy dispatch contract.
    other = Dnsruby::Message.new('different.example.')
    other.header.id = 424
    entry = registry.match(424, other.question)
    assert_equal :update, entry.client_query_id
    assert entry.question_optional
  end

  def test_separate_generations_are_independent
    old = build(generation: 1, capacity: 2)
    new = build(generation: 2, capacity: 2)
    old.reserve(1, :old_query, question)
    # The same header ID on the next connection generation is a distinct owner.
    serial = new.reserve(1, :new_query, question)
    assert_equal 1, serial
    refute old.entry_for(1).equal?(new.entry_for(1))
    assert_equal 1, old.entry_for(1).generation
    assert_equal 2, new.entry_for(1).generation
  end

  def test_discard_returns_every_outstanding_entry_once
    registry = build(capacity: 5)
    [1, 2, 3].each { |id| registry.reserve(id, "q#{id}".to_sym, question) }
    entries = registry.each_entry.to_a
    assert_equal [1, 2, 3], entries.map(&:header_id).sort
    registry.release(2, :q2)
    assert_equal [1, 3], registry.each_entry.map(&:header_id).sort
  end
end
