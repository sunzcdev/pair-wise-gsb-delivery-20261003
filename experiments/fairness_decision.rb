require "socket"
require "puma"
require "puma/thread_pool"
STDOUT.sync = true

T0 = Process.clock_gettime(Process::CLOCK_MONOTONIC)
NOW = -> { ("%.3f" % (Process.clock_gettime(Process::CLOCK_MONOTONIC) - T0)).rjust(6) }

Puma::Server.class_eval do
  define_method(:process_client) do |processor, client|
    close_socket = true
    requests = 0
    begin
      raise client.reactor_error if client.reactor_error
      if @queue_requests && !client.eagerly_finish { shutdown_body_limit }
        client.set_timeout(@first_data_timeout)
        if @reactor.add client
          close_socket = false
          return false
        end
      end
      with_force_shutdown(client) { client.finish(@first_data_timeout) { shutdown_body_limit } }
      can_loop = true
      while can_loop
        can_loop = false
        @requests_count += 1
        case handle_request(processor, client, requests + 1)
        when :close
        when :async
          close_socket = false
        when :keep_alive
          requests += 1
          client.reset
          next_request_ready = if client.has_back_to_back_requests?
            with_force_shutdown(client) { client.process_back_to_back_requests }
          else
            with_force_shutdown(client) { client.eagerly_finish { shutdown_body_limit } }
          end
          if next_request_ready
            if @thread_pool.waiting > 0
              puts "#{NOW.call}  req served=#{client.requests_served}: INLINE loop (waiting=#{@thread_pool.waiting}, spawned=#{@thread_pool.spawned})"
              can_loop = true
            else
              puts "#{NOW.call}  req served=#{client.requests_served}: REQUEUE   (waiting=0, spawned=#{@thread_pool.spawned})"
              @thread_pool.with_mutex do
                unless shutting_down?
                  @thread_pool << client
                  close_socket = false
                end
              end
            end
          elsif @queue_requests
            puts "#{NOW.call}  req served=#{client.requests_served}: to REACTOR (waiting=#{@thread_pool.waiting})"
            client.set_timeout @persistent_timeout
            if @reactor.add client
              close_socket = false
            end
          end
        end
      end
      true
    rescue StandardError => e
      client_error(e, client, requests)
      requests > 0
    ensure
      client.io_buffer.reset
      close_client_safely(client) if close_socket
    end
  end
end

MAX = Integer(ARGV[0] || 2)
app = lambda { |env| sleep 0.12; [200, {"x-req" => env["HTTP_X_REQ"]}, ["ok"]] }
server = Puma::Server.new(app, nil, min_threads: 1, max_threads: MAX,
                                queue_requests: true, log_writer: Puma::LogWriter.strings)
port = server.add_tcp_listener("127.0.0.1", 0).addr[1]
server.run

read_resp = lambda do |s, tag, i|
  s.gets("\r\n")
  while (h = s.gets("\r\n")) != "\r\n"; end
  s.read(2)
  puts "#{NOW.call}  #{tag} got response #{i}"
end

a = b = nil
begin
  a = TCPSocket.new("127.0.0.1", port)
  4.times { |i| a.write("GET /a HTTP/1.1\r\nHost: t\r\nX-Req: A#{i}\r\nContent-Length: 0\r\n\r\n") }
  puts "#{NOW.call}  A pipelined 4 requests (max_threads=#{MAX})"
  sleep 0.06
  b = TCPSocket.new("127.0.0.1", port)
  b.write("GET /b HTTP/1.1\r\nHost: t\r\nX-Req: B0\r\nConnection: close\r\nContent-Length: 0\r\n\r\n")
  puts "#{NOW.call}  B sent single request"
  th = Thread.new { 4.times { |i| read_resp.call(a, "A", i) } }
  read_resp.call(b, "B", 0)
  th.join
ensure
  a&.close
  b&.close
  server.stop(true)
end
