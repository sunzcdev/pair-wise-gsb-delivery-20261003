# frozen_string_literal: true

require "socket"

# An in-process AMQP 0-9-1 protocol stand-in.
#
# It implements just enough of the protocol for a Bunny session to connect,
# open a channel, publish and use tx.* methods. Replies can be withheld on
# purpose to observe how the client reports an outcome it never received.
#
# @private
class BunnyProtocolStandIn
  SERVER_CAPABILITIES = {
    "publisher_confirms" => true,
    "exchange_exchange_bindings" => true,
    "basic.nack" => true,
    "consumer_cancel_notify" => true,
    "connection.blocked" => true,
    "authentication_failure_close" => true
  }.freeze

  # @return [Integer] TCP port the stand-in listens on
  attr_reader :port

  def initialize
    @server           = TCPServer.new("127.0.0.1", 0)
    @port             = @server.addr[1]
    @received         = []
    @published        = []
    @mutex            = Mutex.new
    @write_mutex      = Mutex.new
    @next_commit_ok   = nil
    @stopping         = false
    @connection_threads = []
  end

  def start
    @acceptor = Thread.new do
      until @stopping
        begin
          socket = @server.accept
        rescue IOError, Errno::EBADF
          break
        end

        @connection_threads << Thread.new { serve(socket) }
      end
    end
    self
  end

  def stop
    @stopping = true
    @acceptor&.kill
    @connection_threads.each(&:kill)
    @server.close
  rescue IOError, Errno::EBADF
    # already closed
  end

  # Makes the stand-in accept the next tx.commit but drop the connection instead
  # of replying, like a broker that commits and then crashes before responding.
  def drop_next_commit_ok!
    @mutex.synchronize { @next_commit_ok = :drop }
  end

  # Makes the stand-in accept the next tx.commit but never reply to it while
  # keeping the connection up, like a peer that stalls after committing.
  def withhold_next_commit_ok!
    @mutex.synchronize { @next_commit_ok = :withhold }
  end

  # Makes the stand-in accept the next tx.commit and reply to it after a delay,
  # so that the response arrives after the caller's continuation timeout.
  def delay_next_commit_ok!(seconds = 0.5)
    @mutex.synchronize { @next_commit_ok = [:delay, seconds] }
  end

  # Makes the stand-in accept and then reject the next tx.commit with a
  # channel-level exception, like RabbitMQ does for a non-transactional channel.
  def reject_next_commit!(reply_code:, reply_text:)
    @mutex.synchronize { @next_commit_ok = [:reject, reply_code, reply_text] }
  end

  # @return [Array<Array(Integer, Integer, Integer)>] received methods as [channel, class id, method id]
  def received_methods
    @mutex.synchronize { @received.dup }
  end

  # @return [Array<Array(String, String)>] published messages as [routing key, payload]
  def published_messages
    @mutex.synchronize { @published.dup }
  end

  def received_method_count(class_id, method_id)
    @mutex.synchronize { @received.count { |_, c, m| c == class_id && m == method_id } }
  end

  private

  def serve(socket)
    return if socket.read(8).nil? # protocol preamble

    write_method(socket, 0, start_payload)

    loop do
      frame = read_frame(socket)
      break if frame.nil?

      type, channel, payload = frame
      next unless type == 1 # only method frames are of interest here

      class_id, method_id = payload.byteslice(0, 4).unpack("n2")
      @mutex.synchronize { @received << [channel, class_id, method_id] }

      case [class_id, method_id]
      when [10, 11] # connection.start-ok
        write_method(socket, 0, [10, 30].pack("n2") + [2047].pack("n") + [131_072].pack("N") + [0].pack("n")) # connection.tune
      when [10, 31] then next # connection.tune-ok
      when [10, 40] then write_method(socket, 0, [10, 41].pack("n2") + [0].pack("C")) # connection.open -> open-ok
      when [10, 50] # connection.close
        write_method(socket, channel, [10, 51].pack("n2"))
        break
      when [20, 10] then write_method(socket, channel, [20, 11].pack("n2") + [0].pack("N")) # channel.open -> open-ok
      when [20, 40] # channel.close
        write_method(socket, channel, [20, 41].pack("n2"))
      when [50, 10] then write_method(socket, channel, [50, 11].pack("n2") + "\x00".b + [0, 0].pack("N2")) # queue.declare -> declare-ok
      when [50, 40] then write_method(socket, channel, [50, 41].pack("n2") + [0].pack("N")) # queue.delete -> delete-ok
      when [60, 40] then record_publish(socket, channel, payload) # basic.publish
      when [90, 10] then write_method(socket, channel, [90, 11].pack("n2")) # tx.select -> select-ok
      when [90, 20] # tx.commit
        behavior = next_commit_ok_behavior

        case behavior
        when :drop
          # the commit was accepted, the reply is lost with the connection
          socket.close
          break
        when :withhold
          # the commit was accepted, the reply never comes
          next
        when Array
          kind, first, second = behavior

          case kind
          when :delay
            # the commit was accepted, the reply arrives after the caller gave up
            Thread.new do
              sleep first
              begin
                write_method(socket, channel, [90, 21].pack("n2"))
              rescue IOError, SystemCallError
                # the connection was closed in the meantime
              end
            end
          when :reject
            write_channel_close(socket, channel, first, second)
          end
        else
          write_method(socket, channel, [90, 21].pack("n2"))
        end
      when [90, 30] then write_method(socket, channel, [90, 31].pack("n2")) # tx.rollback -> rollback-ok
      end
    end
  rescue IOError, SystemCallError, EOFError
    # the connection went away, nothing to clean up
  ensure
    begin
      socket.close
    rescue IOError, SystemCallError
      # already closed
    end
  end

  def next_commit_ok_behavior
    @mutex.synchronize do
      behavior = @next_commit_ok
      @next_commit_ok = nil
      behavior
    end
  end

  def start_payload
    mechanisms = "PLAIN"
    locales    = "en_GB"
    [10, 10].pack("n2") +
      [0, 9].pack("CC") +
      AMQ::Protocol::Table.encode("capabilities" => SERVER_CAPABILITIES) +
      [mechanisms.bytesize].pack("N") + mechanisms +
      [locales.bytesize].pack("N") + locales
  end

  # basic.publish is followed by a content header frame and zero or more body frames
  def record_publish(socket, channel, payload)
    offset       = 6 # frame payload starts with class id, method id and the ticket
    exchange_len = payload.getbyte(offset)
    offset += 1
    offset += exchange_len
    rk_len = payload.getbyte(offset)
    offset += 1
    routing_key = payload.byteslice(offset, rk_len)

    header = read_frame(socket)
    return if header.nil?

    body_size = header[2].byteslice(4, 8).unpack1("Q>")
    body      = +""
    while body.bytesize < body_size
      body_frame = read_frame(socket)
      return if body_frame.nil?

      body << body_frame[2]
    end

    @mutex.synchronize { @published << [routing_key, body] }
  end

  def write_method(socket, channel, payload)
    @write_mutex.synchronize do
      socket.write(AMQ::Protocol::MethodFrame.new(payload, channel).encode)
      socket.flush
    end
  end

  # channel.close naming the tx.commit method, followed by the client's close-ok
  def write_channel_close(socket, channel, reply_code, reply_text)
    payload = [20, 40].pack("n2") +
      [reply_code].pack("n") +
      [reply_text.bytesize].pack("C") + reply_text +
      [AMQ::Protocol::Tx.method_id, AMQ::Protocol::Tx::Commit.method_id].pack("n2")
    write_method(socket, channel, payload)

    # the client answers with channel.close-ok
    read_frame(socket)
  end

  # @return [Array(Integer, Integer, String), nil] [type, channel, payload]
  def read_frame(socket)
    header = read_exactly(socket, 7)
    return nil if header.nil?

    type    = header.getbyte(0)
    channel = header.byteslice(1, 2).unpack1("n")
    size    = header.byteslice(3, 4).unpack1("N")
    payload = size.zero? ? +"".b : read_exactly(socket, size)
    return nil if payload.nil?

    final_octet = read_exactly(socket, 1)
    return nil if final_octet.nil?

    [type, channel, payload]
  end

  def read_exactly(socket, count)
    data = +"".b
    while data.bytesize < count
      chunk = socket.read(count - data.bytesize)
      return nil if chunk.nil?

      data << chunk
    end
    data
  end
end
