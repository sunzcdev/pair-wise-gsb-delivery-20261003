require_relative 'spec_helper'
require 'minitest/mock'

# In-memory authoritative answers drive every lookup here; no sockets and no
# real domain names are involved. The traced ResolutionPath records the exact
# names, query identities and edge kinds a recursion follows.
class RecursorResolutionPathTest < Minitest::Test
  def test_successful_alias_chain_records_names_identities_and_edges
    packets = {
      'www.example.' => answer('www.example.', 'www.example. 60 IN CNAME target.other.'),
      'target.other.' => answer('target.other.', 'target.other. 60 IN A 192.0.2.1')
    }
    with_recursor(packets) do |recursor|
      message, path = recursor.query('www.example.', 'A', 'IN', true, trace_path: true) { |msg, p| [msg, p] }
      assert_equal([Dnsruby::Types.CNAME, Dnsruby::Types.A], message.answer.map(&:type))
      assert(path.complete?)
      refute(path.loop?)
      refute(path.incomplete?)
      assert_equal(%i[alias answer], path.hops.map(&:edge))
      questions = path.questions
      assert_equal([Dnsruby::Name.create('www.example.'), Dnsruby::Name.create('target.other.')],
        questions.map(&:first))
      assert(questions.all? { |_name, type, klass| type == Dnsruby::Types.A && klass == Dnsruby::Classes.IN })
      assert_equal('target.other.', path.hops.first.target.to_s(true))
      assert_nil(path.hops.last.target)
    end
  end

  def test_path_is_optional_and_absent_by_default
    packets = { 'host.example.' => answer('host.example.', 'host.example. 60 IN A 192.0.2.2') }
    with_recursor(packets) do |recursor|
      message = recursor.query('host.example.', 'A', 'IN', true)
      assert_equal([Dnsruby::Types.A], message.answer.map(&:type))
      assert_nil(Dnsruby::Recursor.send(:current_path))
    end
  end

  def test_self_loop_is_classified_and_keeps_the_confirmed_cname
    packets = {
      'loop.example.' => answer('loop.example.', 'loop.example. 60 IN CNAME loop.example.')
    }
    with_recursor(packets) do |recursor|
      error = assert_raises(Dnsruby::RecursionLimitError) do
        recursor.query('loop.example.', 'A', 'IN', true, trace_path: true)
      end
      path = error.resolution_path
      assert_equal(:self_loop, path.terminal)
      assert(path.loop?)
      assert(path.incomplete?)
      assert_equal(%i[alias], path.aliases.map(&:edge))
      assert_equal('loop.example.', path.aliases.first.target.to_s(true))
      assert(error.response.incomplete?)
      assert_equal('loop.example.', error.response.question.first.qname.to_s(true))
      assert_equal([Dnsruby::Types.CNAME], error.response.answer.map(&:type))
    end
  end

  def test_indirect_loop_is_distinct_from_a_self_loop
    packets = {
      'a.example.' => answer('a.example.', 'a.example. 60 IN CNAME b.example.'),
      'b.example.' => answer('b.example.', 'b.example. 60 IN CNAME a.example.')
    }
    with_recursor(packets) do |recursor|
      error = assert_raises(Dnsruby::RecursionLimitError) do
        recursor.query('a.example.', 'A', 'IN', true, trace_path: true)
      end
      path = error.resolution_path
      assert_equal(:indirect_loop, path.terminal)
      assert(path.loop?)
      owners = path.questions.map { |name, _type, _klass| name.to_s(true) }
      assert_equal(%w[a.example. b.example. a.example.], owners)
      assert(error.response.incomplete?)
      # The closing edge re-queries the already-seen identity a.example.; it
      # is retained on the path so the repeated owner is visible.
      assert_equal(3, path.aliases.length)
      assert_equal('a.example.', path.aliases.last.question.first.to_s(true))
      assert_equal('b.example.', path.aliases.last.target.to_s(true))
    end
  end

  def test_hop_limit_terminates_a_long_chain_with_a_distinct_terminal
    original = Dnsruby::Recursor::MAX_ALIAS_HOPS
    Dnsruby::Recursor.send(:remove_const, :MAX_ALIAS_HOPS)
    Dnsruby::Recursor.const_set(:MAX_ALIAS_HOPS, 2)
    packets = {
      'a.example.' => answer('a.example.', 'a.example. 60 IN CNAME b.example.'),
      'b.example.' => answer('b.example.', 'b.example. 60 IN CNAME c.example.'),
      'c.example.' => answer('c.example.', 'c.example. 60 IN CNAME d.example.')
    }
    with_recursor(packets) do |recursor|
      error = assert_raises(Dnsruby::RecursionLimitError) do
        recursor.query('a.example.', 'A', 'IN', true, trace_path: true)
      end
      assert_equal(:hop_limit, error.resolution_path.terminal)
      refute(error.resolution_path.loop?)
      assert_equal(3, error.resolution_path.aliases.length)
      assert(error.response.incomplete?)
    end
  ensure
    Dnsruby::Recursor.send(:remove_const, :MAX_ALIAS_HOPS)
    Dnsruby::Recursor.const_set(:MAX_ALIAS_HOPS, original)
  end

  def test_same_name_under_another_rr_type_is_not_an_alias_loop
    path = Dnsruby::Recursor::ResolutionPath.new
    assert_equal(:ok, path.record_alias('host.example.', 'A', 'IN', 'target.example.'))
    # The repeated owner name carries a different query identity (AAAA).
    assert_equal(:ok, path.record_alias('host.example.', 'AAAA', 'IN', 'target6.example.'))
    assert_equal(2, path.aliases.length)
    refute(path.loop?)
  end

  def test_referral_edge_is_never_treated_as_an_alias_edge
    path = Dnsruby::Recursor::ResolutionPath.new
    path.add_referral('host.example.', 'A', 'IN', 'sub.example.', '.')
    # The same (name, type, class) as an alias must still be a fresh alias edge.
    assert_equal(:ok, path.record_alias('host.example.', 'A', 'IN', 'target.example.'))
    assert_equal(%i[referral alias], path.hops.map(&:edge))
    assert_equal('sub.example.', path.hops.first.target.to_s(true))
    assert_equal('.', path.hops.first.zone)
  end

  def test_referral_then_alias_records_both_edge_kinds_in_one_lookup
    # The first hop is an authoritative referral (no answer, NS in authority).
    # _dorecursion follows that referral internally and continues at the alias.
    referral = Dnsruby::Message.new('www.child.example.', 'A')
    referral.add_authority(Dnsruby::RR.create('child.example. 60 IN NS ns.child.example.'))
    alias_answer = answer('www.child.example.', 'www.child.example. 60 IN CNAME final.example.')
    final_answer = answer('final.example.', 'final.example. 60 IN A 192.0.2.9')
    sequence = [referral, alias_answer, final_answer]
    with_recursor({}) do |recursor|
      recursor.singleton_class.send(:remove_method, :_dorecursion)
      recursor.define_singleton_method(:_dorecursion) do |name, type, klass, known_zone, authorities, depth, no_validation, path: nil|
        packet = sequence.shift
        if packet.equal?(referral)
          # Mirror the real referral branch: record the delegation, cache the
          # zone, then recurse against the delegated authority.
          path&.add_referral(name, type, klass, 'child.example.', known_zone)
          _dorecursion(name, type, klass, 'child.example.', authorities, depth + 1, no_validation, path: path)
        else
          packet
        end
      end
      message, path = recursor.query('www.child.example.', 'A', 'IN', true, trace_path: true) { |msg, p| [msg, p] }
      edges = path.hops.map(&:edge)
      assert_equal(%i[referral alias answer], edges)
      assert(path.complete?)
      assert_equal([Dnsruby::Types.CNAME, Dnsruby::Types.A], message.answer.map(&:type))
      assert_equal('child.example.', path.hops.first.target.to_s(true))
    end
  end

  def test_mid_chain_failure_keeps_confirmed_results_and_marks_path_incomplete
    packets = {
      'www.example.' => answer('www.example.', 'www.example. 60 IN CNAME gone.other.')
    }
    with_recursor(packets) do |recursor|
      recursor.singleton_class.send(:remove_method, :_dorecursion)
      recursor.define_singleton_method(:_dorecursion) do |name, *_args, **_kwargs|
        if name == 'gone.other.'
          nxd = Dnsruby::Message.new('gone.other.', 'A')
          nxd.header.rcode = Dnsruby::RCode::NXDOMAIN
          nxd.add_authority(Dnsruby::RR.create('other. 60 IN SOA ns.other. hostmaster.other. 1 60 60 60 30'))
          error = Dnsruby::NXDomain.new('gone.other.')
          error.response = nxd
          raise error
        end
        packets.fetch(name)
      end
      error = assert_raises(Dnsruby::NXDomain) do
        recursor.query('www.example.', 'A', 'IN', true, trace_path: true)
      end
      path = error.resolution_path
      assert_equal(:failure, path.terminal)
      assert(path.incomplete?)
      assert_equal(%i[alias failure], path.hops.map(&:edge))
      assert(error.response.incomplete?)
      # The confirmed prefix is retained, under the original question.
      assert_equal('www.example.', error.response.question.first.qname.to_s(true))
      assert_equal([Dnsruby::Types.CNAME], error.response.answer.map(&:type))
    end
  end

  def test_incomplete_alias_chain_is_never_cached
    packets = {
      'loop.example.' => answer('loop.example.', 'loop.example. 60 IN CNAME loop.example.')
    }
    with_recursor(packets, caching: true) do |recursor|
      cache = recursor.instance_variable_get(:@response_cache)
      query = Dnsruby::Message.new('loop.example.', 'A', 'IN')
      assert_raises(Dnsruby::RecursionLimitError) do
        recursor.query('loop.example.', 'A', 'IN', true, trace_path: true)
      end
      assert_nil(cache.find(query, :scope), 'A partial success chain must not enter the cache')
    end
  end

  def test_resolution_cache_rejects_an_incomplete_response_directly
    cache = Dnsruby::Cache::ResolutionCache.new
    query = Dnsruby::Message.new('partial.example.', 'A', 'IN')
    response = negative_response('partial.example.')
    response.incomplete = true
    error = Dnsruby::NXDomain.new('partial.example.')
    error.response = response
    cache.add(query, :scope, response, error)
    assert_nil(cache.find(query, :scope))
    # The same authoritative negative answer, once complete, is admitted.
    response.incomplete = false
    cache.add(query, :scope, response, error)
    assert(cache.find(query, :scope))
  end

  def test_complete_negative_answers_are_cached_normally
    packets = {
      'missing.example.' => negative_response('missing.example.')
    }
    with_recursor(packets, caching: true) do |recursor|
      recursor.query('missing.example.', 'A', 'IN', true, trace_path: true)
      cache = recursor.instance_variable_get(:@response_cache)
      query = Dnsruby::Message.new('missing.example.', 'A', 'IN')
      stored, error = cache.find(query, :scope)
      refute_nil(stored, 'A completed negative answer remains cacheable')
      assert_nil(error)
      assert_equal(Dnsruby::RCode::NXDOMAIN, stored.rcode)
      refute(stored.incomplete?)
    end
  end

  def test_dname_synthesized_alias_is_recorded_as_an_alias_edge
    packets = {
      'host.branch.example.' => answer('host.branch.example.', 'branch.example. 60 IN DNAME other.example.'),
      'host.other.example.' => answer('host.other.example.', 'host.other.example. 60 IN A 192.0.2.3')
    }
    with_recursor(packets) do |recursor|
      _message, path = recursor.query('host.branch.example.', 'A', 'IN', true, trace_path: true) { |msg, p| [msg, p] }
      assert_equal(%i[alias answer], path.hops.map(&:edge))
      assert_equal('host.other.example.', path.aliases.first.target.to_s(true))
      assert(path.complete?)
    end
  end

  def test_explicit_cname_query_does_not_chase_and_records_an_answer
    packets = {
      'alias.example.' => answer('alias.example.', 'alias.example. 60 IN CNAME target.example.')
    }
    with_recursor(packets) do |recursor|
      _message, path = recursor.query('alias.example.', 'CNAME', 'IN', true, trace_path: true) { |msg, p| [msg, p] }
      assert_equal(%i[answer], path.hops.map(&:edge))
      assert(path.complete?)
    end
  end

  private

  def answer(name, *records)
    Dnsruby::Message.new(name).tap do |packet|
      packet.header.qr = packet.header.aa = true
      records.each { |record| packet.add_answer(Dnsruby::RR.create(record)) }
    end
  end

  def negative_response(name)
    Dnsruby::Message.new(name, 'A').tap do |packet|
      packet.header.qr = true
      packet.header.rcode = Dnsruby::RCode::NXDOMAIN
      packet.add_authority(Dnsruby::RR.create(
        'example. 60 IN SOA ns.example. hostmaster.example. 1 60 60 60 30'))
    end
  end

  def with_recursor(packets, caching: false)
    recursor = Dnsruby::Recursor.allocate
    recursor.instance_variable_set(:@resolver, Struct.new(:query_timeout, :recurse).new(10, false))
    recursor.instance_variable_set(:@ipv6_ok, false)
    recursor.instance_variable_set(:@dnssec, false)
    recursor.instance_variable_set(:@response_cache, Dnsruby::Cache::ResolutionCache.new)
    if caching
      recursor.define_singleton_method(:recursion_cache_scope) { |_no_validation| :scope }
    else
      recursor.define_singleton_method(:recursion_cache_scope) { |_no_validation| nil }
    end
    recursor.define_singleton_method(:get_closest_known_zone_authorities_for) { |_name| ['.', {}] }
    recursor.define_singleton_method(:_dorecursion) do |name, *_args, path: nil, **_kwargs|
      packets.fetch(name)
    end
    Dnsruby::Recursor.stub(:ensure_hints, 0) { yield recursor }
  end
end
