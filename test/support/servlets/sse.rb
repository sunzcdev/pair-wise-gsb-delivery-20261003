# frozen_string_literal: true

require_relative "test"

class SSE < TestHTTP2Server
  # +chunk_size+ forces the event stream to be written in slices of that many bytes, which
  # makes it possible to exercise a client's incremental parsing across byte boundaries
  # (including multi-byte characters and lines split in the middle).
  #
  # +trailing+ is appended to the stream after the last message, without a terminating
  # blank line (i.e. a stream which ends in the middle of an event).
  def initialize(messages: [], close_after: nil, chunk_size: nil, trailing: nil, **kwargs)
    super(**kwargs)
    @messages = messages
    @close_after = close_after
    @chunk_size = chunk_size
    @trailing = trailing
    @already_closed = false
  end

  private

  def handle_connection(conn, sock)
    case sock
    when TCPSocket
      handle_http1_stream(sock)
    else
      case sock.alpn_protocol
      when "h2"
        super
      else
        handle_http1_stream(sock)
      end
    end
  end

  def handle_http1_stream(sock)
    data = sock.readpartial(2048) # enough to read headers
    lines = data.split("\r\n")
    lines.shift # reject GET etc
    h = lines.reject(&:empty?).to_h do |line|
      line.split(/ *: */, 2)
    end

    event_stream = h["Accept"] == "text/event-stream"
    last_event_id = h["Last-Event-ID"] if h.key?("Last-Event-ID")

    if event_stream
      write_event_bytes(sock, "HTTP/1.1 200 OK\r\n" \
                              "Connection: keep-alive\r\n" \
                              "Cache-Control: no-cache\r\n" \
                              "Content-Type: text/event-stream\r\n\r\n")
      @messages.each do |msg|
        if last_event_id
          last_event_id = nil if msg[:id] && last_event_id == msg[:id].to_s

          next # skip message
        end

        if (comment = msg[:comment])
          write_event_bytes(sock, ": #{comment}\n")
        end

        if (event = msg[:event])
          write_event_bytes(sock, "event: #{event}\n")
        end

        case (data = msg[:data])
        when Array
          data.each { |d| write_event_bytes(sock, "data: #{d}\n") }
        else
          write_event_bytes(sock, "data: #{data}\n")
        end

        if (id = msg[:id])
          write_event_bytes(sock, "id: #{id}\n")
        end

        if (retry_after = msg[:retry])
          write_event_bytes(sock, "retry: #{retry_after}\n")
        end

        write_event_bytes(sock, "\n")

        next unless @close_after && @close_after == id && !@already_closed

        write_event_bytes(sock, "incompleteline")
        break
      end

      write_event_bytes(sock, @trailing) if @trailing

      sock.close
    else
      sock.write "HTTP/1.1 200 OK\r\n" \
                 "Connection: close\r\n" \
                 "Content-Type: text/plain\r\n" \
                 "Content-Length: 2\r\n\r\nOK"
    end
  end

  # writes +bytes+ to +sock+, sliced in +@chunk_size+ chunks when set, so that the client
  # is made to parse the stream across arbitrary byte boundaries.
  def write_event_bytes(sock, bytes)
    return sock.write(bytes) unless @chunk_size

    offset = 0
    while offset < bytes.bytesize
      sock.write(bytes.byteslice(offset, @chunk_size))
      offset += @chunk_size
    end
  end

  def handle_stream(_sock, conn, stream)
    event_stream = false
    last_event_id = nil
    stream.on(:headers) do |h|
      h = Hash[*h.flatten]
      event_stream = h["accept"] == "text/event-stream"
      last_event_id = h["last-event-id"] if h.key?("last-event-id")
    end

    stream_already_closed = false
    stream.on(:half_close) do
      if event_stream
        stream.headers({
                         ":status" => "200",
                         "cache-control" => "no-cache",
                         "content-type" => "text/event-stream",
                       }, end_stream: false)

        @messages.each do |msg|
          if last_event_id
            last_event_id = nil if msg[:id] && last_event_id == msg[:id].to_s

            next # skip message

          end

          if (comment = msg[:comment])
            stream.data(": #{comment}\n", end_stream: false)
          end

          if (event = msg[:event])
            stream.data("event: #{event}\n", end_stream: false)
          end

          case (data = msg[:data])
          when Array
            data.each { |d| stream.data("data: #{d}\n", end_stream: false) }
          else
            stream.data("data: #{data}\n", end_stream: false)
          end

          if (id = msg[:id])
            stream.data("id: #{id}\n", end_stream: false)
          end

          if (retry_after = msg[:retry])
            stream.data("retry: #{retry_after}\n", end_stream: false)
          end

          stream.data("\n", end_stream: false)

          next unless @close_after && @close_after == id && !@already_closed

          stream.close
          conn.goaway(:internal_error)
          stream_already_closed = true
          break
        end

        stream.data(@trailing, end_stream: false) if @trailing && !stream_already_closed

        stream.data("", end_stream: true) unless stream_already_closed
      else
        response = "OK"
        stream.headers({
                         ":status" => "200",
                         "content-length" => response.bytesize.to_s,
                         "content-type" => "text/plain",
                       }, end_stream: false)
        stream.data(response, end_stream: true)
      end
    end
  end
end
