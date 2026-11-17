require_relative 'spec_helper'
require 'timeout'

class ResolverConfigurationContractsTest < Minitest::Test
  def test_single_resolver_keeps_only_its_configured_server
    Dnsruby::Config.stub(:default_config_hash, {nameserver: %w[192.0.2.1 192.0.2.2]}) do
      resolver = Dnsruby::SingleResolver.new
      assert_equal(['192.0.2.1'], resolver.single_resolvers.map(&:server))
      resolver.server = '198.51.100.1'
      assert_equal(['198.51.100.1'], resolver.single_resolvers.map(&:server))
      assert_equal('198.51.100.1', resolver.server)
      assert_equal(['198.51.100.1'], resolver.config.nameserver)
    ensure
      resolver&.close
    end
  end

  def test_zero_attempt_plan_completes_once_without_sending
    resolver = Dnsruby::Resolver.new(nameserver: '127.0.0.1', retry_times: 0, query_timeout: 0, do_caching: false)
    queue = Queue.new
    resolver.single_resolvers.first.define_singleton_method(:prepare_send_async) { |*| raise 'Unexpected send' }
    resolver.send_async(Dnsruby::Message.new('example.'), queue, :empty)
    id, response, error = Timeout.timeout(1) { queue.pop }
    assert_equal(:empty, id)
    assert_nil(response)
    assert_kind_of(Dnsruby::ResolvTimeout, error)
    assert(queue.empty?)
  ensure
    resolver&.close
  end
end
