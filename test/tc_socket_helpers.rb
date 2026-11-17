require_relative 'spec_helper'
require_relative 'socket_helpers'
require 'minitest/mock'

class TestSocketHelpers < Minitest::Test
  def setup
    @tcp_sockets = []
    @udp_sockets = []
  end

  def teardown
    (@tcp_sockets + @udp_sockets).each { |socket| socket.close unless socket.closed? }
  end

  def test_collision_retries_without_releasing_the_successful_pair
    blocker, first_tcp = TestSockets.udp_tcp_pair
    blocked_port = blocker.addr[1]
    tcp_new = TCPServer.method(:new)
    udp_new = UDPSocket.method(:new)
    tcp_factory = lambda do |*args|
      socket = @tcp_sockets.empty? ? first_tcp : tcp_new.call(*args)
      @tcp_sockets << socket
      socket
    end
    udp_factory = ->(*args) { udp_new.call(*args).tap { |socket| @udp_sockets << socket } }
    TCPServer.stub(:new, tcp_factory) do
      UDPSocket.stub(:new, udp_factory) do
        udp, tcp = TestSockets.udp_tcp_pair
        assert_operator(@tcp_sockets.length, :>=, 2)
        assert(first_tcp.closed?)
        assert(@udp_sockets.first.closed?)
        assert_equal(udp.addr[1], tcp.addr[1])
        refute_equal(blocked_port, tcp.addr[1])
        refute(udp.closed?)
        refute(tcp.closed?)
        refute(blocker.closed?)
      end
    end
  ensure
    blocker&.close unless blocker&.closed?
    first_tcp&.close unless first_tcp&.closed?
  end

  def test_repeated_collisions_are_bounded_and_close_every_attempt
    with_bind_error(Errno::EADDRINUSE) do
      assert_raises(Errno::EADDRINUSE) { TestSockets.udp_tcp_pair }
    end
    assert_equal(10, @tcp_sockets.length)
    assert_equal(10, @udp_sockets.length)
    assert((@tcp_sockets + @udp_sockets).all?(&:closed?))
  end

  def test_other_bind_errors_are_not_retried
    with_bind_error(Errno::EACCES) do
      assert_raises(Errno::EACCES) { TestSockets.udp_tcp_pair }
    end
    assert_equal(1, @tcp_sockets.length)
    assert_equal(1, @udp_sockets.length)
    assert((@tcp_sockets + @udp_sockets).all?(&:closed?))
  end

  def test_udp_creation_failure_releases_the_tcp_listener
    tcp_new = TCPServer.method(:new)
    factory = ->(*args) { tcp_new.call(*args).tap { |socket| @tcp_sockets << socket } }
    TCPServer.stub(:new, factory) do
      UDPSocket.stub(:new, ->(*) { raise Errno::EMFILE }) do
        assert_raises(Errno::EMFILE) { TestSockets.udp_tcp_pair }
      end
    end
    assert_equal(1, @tcp_sockets.length)
    assert(@tcp_sockets.first.closed?)
  end

  private

  def with_bind_error(error)
    tcp_new = TCPServer.method(:new)
    udp_new = UDPSocket.method(:new)
    tcp_factory = ->(*args) { tcp_new.call(*args).tap { |socket| @tcp_sockets << socket } }
    udp_factory = lambda do |*args|
      udp_new.call(*args).tap do |socket|
        @udp_sockets << socket
        socket.define_singleton_method(:bind) { |*| raise error }
      end
    end
    TCPServer.stub(:new, tcp_factory) do
      UDPSocket.stub(:new, udp_factory) { yield }
    end
  end
end
