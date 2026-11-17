# frozen_string_literal: true

require_relative 'spec_helper'
require 'open3'
require 'rbconfig'

# End-to-end behaviour of the explicit TCP message ID reservation boundary using
# an in-process transport double: no real sockets, but the real PacketSender
# admission path and the real SelectThread registry/dispatch path run together.
# Each scenario owns a fresh selector inside a subprocess so its fake descriptors
# and class-variable tables cannot reach a live singleton.
class TestTcpMessageIdLifecycle < Minitest::Test
  FIXTURE = <<~'RUBY'
    require 'dnsruby'
    require 'timeout'
    Dnsruby.log.level = Logger::FATAL
    K = Dnsruby::SelectThread
    {
      mutex: Mutex.new, query_hash: {}, queries_by_queue: {}.compare_by_identity, socket_hash: {},
      timeouts: {}, sockets: Set.new, tcp_buffers: {}, socket_is_persistent: {},
      socket_remaining_queries: {}, socket_owners: {}, retiring_sockets: Set.new,
      queued_exceptions: [], queued_responses: [], queued_validation_responses: [],
      observers: {}, observer_queue_counts: {}.compare_by_identity, tick_observers: [],
      select_thread: nil
    }.each { |name, value| K.class_variable_set("@@#{name}", value) }
    SELECTOR = K.send(:allocate)
    SELECTOR.define_singleton_method(:check_select_thread_synchronized) {}
    SELECTOR.define_singleton_method(:wake_up_select_thread) {}
    SELECTOR.define_singleton_method(:notify_queue_observers) { |*| }
    K.define_singleton_method(:instance) { SELECTOR }

    class StreamDouble
      attr_reader :written, :generation
      attr_accessor :incoming
      def initialize
        @closed = false
        @written = ''.b
        @incoming = ''.b
        @generation = nil
      end
      def closed?; @closed; end
      def close; @closed = true; end
      def getsockopt(*)
        raise IOError, 'closed stream' if closed?
        Struct.new(:data).new([Socket::SOCK_STREAM].pack('i'))
      end
      def write_nonblock(bytes)
        raise IOError, 'closed stream' if closed?
        @written << bytes
        bytes.bytesize
      end
      def recv_nonblock(length)
        raise IOError, 'closed stream' if closed?
        raise IO::EAGAINWaitReadable if @incoming.empty?
        @incoming.slice!(0, length)
      end
    end

    def query(name)
      q = Dnsruby::Message.new(name)
      q.do_validation = q.do_caching = false
      q
    end

    def sender(socket, capacity: Dnsruby::TcpMessageRegistry::CAPACITY)
      writer = Dnsruby::PacketSender.new(server: '127.0.0.1', tcp_pipelining: true,
        tcp_pipelining_max_queries: :infinite, packet_timeout: 5, dnssec: false)
      writer.define_singleton_method(:tcp_pipeline_socket) do |_|
        SELECTOR.begin_tcp_generation(socket, capacity) unless SELECTOR.tcp_generation(socket)
        socket.instance_variable_set(:@generation, SELECTOR.tcp_generation(socket))
        [socket, false]
      end
      writer
    end

    def deliver(socket, response)
      wire = response.encode
      socket.incoming = [wire.bytesize].pack('n') + wire
      incoming = SELECTOR.get_incoming_data(socket, 0)
      SELECTOR.process_message(*incoming, socket) if incoming
      SELECTOR.send_queued_validation_responses
    end

    def response_for(message)
      message.clone.tap { |answer| answer.header.qr = true }
    end

    def drain
      SELECTOR.send_queued_exceptions
    end
  RUBY

  def test_out_of_order_responses_reach_their_own_queries
    run_scenario(<<~'RUBY')
      socket = StreamDouble.new
      writer = sender(socket)
      first, second = query('first.example.'), query('second.example.')
      q1, q2 = Queue.new, Queue.new
      writer.send_dnsruby(first.encode, first, :one, q1, true)
      writer.send_dnsruby(second.encode, second, :two, q2, true)
      raise 'both queries must share one generation' unless socket.generation == SELECTOR.tcp_generation(socket)
      # Answer the second transaction first; IDs and questions must keep it honest.
      deliver(socket, response_for(second))
      deliver(socket, response_for(first))
      event_two = q2.pop(true)
      event_one = q1.pop(true)
      raise 'second query got wrong answer' unless event_two[2].question.first.qname.to_s == 'second.example'
      raise 'first query got wrong answer' unless event_one[2].question.first.qname.to_s == 'first.example'
      raise 'queries retained' if SELECTOR.query_settings_for_id(:one) || SELECTOR.query_settings_for_id(:two)
      raise 'registry leaked reservations' unless SELECTOR.discard_tcp_generation(socket).empty?
    RUBY
  end

  def test_question_mismatch_on_same_id_is_not_delivered
    run_scenario(<<~'RUBY')
      socket = StreamDouble.new
      writer = sender(socket)
      held = query('held.example.')
      held.header.id = 4242
      queue = Queue.new
      writer.send_dnsruby(held.encode, held, :held, queue, true)
      # A response that reuses the live ID but asks a different question.
      imposter = query('imposter.example.')
      imposter.header.id = 4242
      imposter.header.qr = true
      deliver(socket, imposter)
      raise 'imposter was delivered' unless queue.empty?
      raise 'genuine reservation was consumed' unless SELECTOR.query_settings_for_id(:held)
      # The genuine answer still completes the transaction.
      deliver(socket, response_for(held))
      event = queue.pop(true)
      raise 'genuine answer lost' unless event[2].question.first.qname.to_s == 'held.example'
    RUBY
  end

  def test_id_wraparound_reuses_only_released_ids_with_new_serials
    run_scenario(<<~'RUBY')
      socket = StreamDouble.new
      writer = sender(socket)
      message = query('wrap.example.')
      message.header.id = 99
      queue = Queue.new
      writer.send_dnsruby(message.encode, message, :wrap, queue, true)
      first_serial = SELECTOR.query_settings_for_id(:wrap).tcp_id_serial
      raise 'missing initial reservation' unless first_serial
      deliver(socket, response_for(message))
      queue.pop(true)
      # The answer released the header ID; a later transaction may wrap onto it.
      writer.send_dnsruby(message.encode, message, :wrap, Queue.new, true)
      second = SELECTOR.query_settings_for_id(:wrap)
      raise 'wrapped reservation missing' unless second
      raise 'serial was reused on wraparound' unless second.tcp_id_serial > first_serial
      raise 'header ID changed' unless second.query.header.id == 99
    RUBY
  end

  def test_late_response_after_timeout_cannot_reach_a_new_query_on_a_fresh_generation
    run_scenario(<<~'RUBY')
      old_socket = StreamDouble.new
      writer = sender(old_socket)
      slow = query('slow.example.')
      slow.header.id = 31337
      slow_queue = Queue.new
      writer.send_dnsruby(slow.encode, slow, :slow, slow_queue, true)
      old_generation = SELECTOR.tcp_generation(old_socket)
      settings = SELECTOR.query_settings_for_id(:slow)
      settings.endtime = Dnsruby::TransportClock.now - 1
      K.class_variable_get(:@@timeouts)[:slow] = settings.endtime
      SELECTOR.instance_variable_get(:@packet_deadlines)[:slow] = settings.endtime
      SELECTOR.process_timeouts
      timeout_event = slow_queue.pop(true)
      raise 'timeout not delivered' unless timeout_event.last.is_a?(Dnsruby::ResolvTimeout)
      # Timeout retired the generation; its registry no longer owns the ID.
      raise 'timed-out registry retained' if SELECTOR.tcp_generation(old_socket)
      new_socket = StreamDouble.new
      writer.define_singleton_method(:tcp_pipeline_socket) { |_| [new_socket, true] }
      fresh = query('fresh.example.')
      fresh.header.id = 31337
      fresh_queue = Queue.new
      writer.send_dnsruby(fresh.encode, fresh, :fresh, fresh_queue, true)
      new_generation = SELECTOR.tcp_generation(new_socket)
      raise 'connection generation did not advance' unless new_generation && new_generation > old_generation
      # The old, late answer arrives on the retired stream and has no owner.
      deliver(old_socket, response_for(slow))
      raise 'fresh query received a late answer' unless fresh_queue.empty?
      raise 'fresh reservation was consumed' unless SELECTOR.query_settings_for_id(:fresh)
      deliver(new_socket, response_for(fresh))
      raise 'fresh answer lost' unless fresh_queue.pop(true)[2].question.first.qname.to_s == 'fresh.example'
    RUBY
  end

  def test_cancellation_releases_the_id_and_retires_its_stream
    run_scenario(<<~'RUBY')
      socket = StreamDouble.new
      writer = sender(socket)
      message = query('cancel.example.')
      queue = Queue.new
      writer.send_dnsruby(message.encode, message, :cancel, queue, true)
      raise 'id was not reserved' if SELECTOR.tcp_generation(socket).nil?
      SELECTOR.remove_id(:cancel)
      drain
      raise 'cancelled query retained' if SELECTOR.query_settings_for_id(:cancel)
      raise 'cancelled generation not reclaimed' if SELECTOR.tcp_generation(socket)
      raise 'stream not retired' unless socket.closed?
    RUBY
  end

  def test_disconnect_reclaims_every_outstanding_reservation_once
    run_scenario(<<~'RUBY')
      socket = StreamDouble.new
      writer = sender(socket)
      queues = {}
      [:a, :b, :c].each do |id|
        queues[id] = Queue.new
        message = query("#{id}.example.")
        writer.send_dnsruby(message.encode, message, id, queues[id], true)
      end
      # Simulate the selector's EOF path: close the descriptor, then let the
      # selector retire it and fail every outstanding owner exactly once.
      socket.close
      exceptions = SELECTOR.clean_up_closed_sockets
      raise 'each owner must produce one EOF failure' unless exceptions.map(&:last).sort == [:a, :b, :c]
      exceptions.each { |error, descriptor, id| SELECTOR.send_exception_to_client(error, descriptor, id) }
      drain
      [:a, :b, :c].each do |id|
        event = queues[id].pop(true)
        raise "query #{id} not failed on EOF" unless event.last.is_a?(Dnsruby::SocketEofResolvError)
        raise 'query retained after EOF' if SELECTOR.query_settings_for_id(id)
      end
      raise 'registry still present after disconnect' if SELECTOR.tcp_generation(socket)
    RUBY
  end

  def test_saturated_registry_rejects_without_overwriting_live_entries
    run_scenario(<<~'RUBY')
      socket = StreamDouble.new
      # A deliberately tiny registry models exhausting all 16-bit IDs.
      writer = sender(socket, capacity: 2)
      queues = {}
      [:first, :second].each do |id|
        queues[id] = Queue.new
        message = query("#{id}.example.")
        message.header.id = id == :first ? 1 : 2
        writer.send_dnsruby(message.encode, message, id, queues[id], true)
      end
      overflow = query('overflow.example.')
      overflow.header.id = 3
      overflow_queue = Queue.new
      writer.send_dnsruby(overflow.encode, overflow, :overflow, overflow_queue, true)
      drain
      event = overflow_queue.pop(true)
      raise 'overflow was not rejected' unless event.last.is_a?(Dnsruby::TcpPipelineSaturated)
      raise 'live reservation overwritten' unless SELECTOR.query_settings_for_id(:first) &&
        SELECTOR.query_settings_for_id(:second)
      raise 'overflow query retained' if SELECTOR.query_settings_for_id(:overflow)
      # Existing owners remain answerable; their reservations were not disturbed.
      first = SELECTOR.query_settings_for_id(:first)
      first_message = first.query
      deliver(socket, response_for(first_message))
      raise 'existing owner could not be answered' unless queues[:first].pop(true)[2]
    RUBY
  end

  def test_header_id_collision_moves_to_a_new_generation_without_overwriting
    run_scenario(<<~'RUBY')
      first_socket = StreamDouble.new
      writer = sender(first_socket)
      first = query('first.example.')
      first.header.id = 5555
      first_queue = Queue.new
      writer.send_dnsruby(first.encode, first, :first, first_queue, true)
      first_generation = SELECTOR.tcp_generation(first_socket)
      raise 'first generation missing' unless first_generation

      # The second transaction reuses the live header ID on purpose. Its sender
      # must abandon this stream reference and open a second generation instead
      # of overwriting the first reservation.
      second_socket = StreamDouble.new
      sockets = [first_socket, second_socket]
      writer.define_singleton_method(:tcp_pipeline_socket) do |_|
        target = sockets.find { |candidate| !SELECTOR.tcp_message_id_in_use?(candidate, 5555) }
        target ||= second_socket
        [target, target.equal?(second_socket)]
      end
      second = query('second.example.')
      second.header.id = 5555
      second_queue = Queue.new
      writer.send_dnsruby(second.encode, second, :second, second_queue, true)
      second_generation = SELECTOR.tcp_generation(second_socket)
      raise 'collision did not open a new generation' unless second_generation && second_generation > first_generation
      raise 'first reservation was overwritten' unless SELECTOR.query_settings_for_id(:first).socket == first_socket
      raise 'second query landed on the colliding stream' unless SELECTOR.query_settings_for_id(:second).socket == second_socket

      deliver(first_socket, response_for(first))
      deliver(second_socket, response_for(second))
      raise 'first answer lost' unless first_queue.pop(true)[2].question.first.qname.to_s == 'first.example'
      raise 'second answer lost' unless second_queue.pop(true)[2].question.first.qname.to_s == 'second.example'
    RUBY
  end

  def test_timeout_retirement_destroys_the_generation_before_any_id_reuse
    run_scenario(<<~'RUBY')
      socket = StreamDouble.new
      writer = sender(socket)
      timed = query('timed.example.')
      timed.header.id = 777
      timed_queue = Queue.new
      writer.send_dnsruby(timed.encode, timed, :timed, timed_queue, true)
      generation = SELECTOR.tcp_generation(socket)
      settings = SELECTOR.query_settings_for_id(:timed)
      settings.endtime = Dnsruby::TransportClock.now - 1
      K.class_variable_get(:@@timeouts)[:timed] = settings.endtime
      SELECTOR.instance_variable_get(:@packet_deadlines)[:timed] = settings.endtime
      SELECTOR.process_timeouts
      raise 'timeout not delivered' unless timed_queue.pop(true).last.is_a?(Dnsruby::ResolvTimeout)
      raise 'stream not retired after unanswered timeout' unless socket.closed?
      raise 'generation survived timeout' if SELECTOR.tcp_generation(socket)
      raise 'timed-out id still reported in use' if SELECTOR.tcp_message_id_in_use?(socket, 777)
      # A late frame on the closed stream has no registry owner and is ignored.
      late = response_for(timed)
      late.header.qr = true
      socket.incoming = [late.encode.bytesize].pack('n') + late.encode
      raise 'late frame produced data on a retired stream' unless SELECTOR.get_incoming_data(socket, 0).nil?
      raise 'generation was recreated by a late frame' if SELECTOR.tcp_generation(socket)
    RUBY
  end

  private

  def run_scenario(script)
    options = ''.frozen? ? ['--enable-frozen-string-literal'] : []
    stdout, stderr, status = Open3.capture3(RbConfig.ruby, *options, '-I',
      File.expand_path('../lib', __dir__), '-e', "#{FIXTURE}\nTimeout.timeout(15) do\n#{script}\nend")
    assert(status.success?, "#{stdout}\n#{stderr}")
    assert_empty(stderr)
  end
end

