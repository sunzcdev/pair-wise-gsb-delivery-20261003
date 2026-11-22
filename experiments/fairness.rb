# frozen_string_literal: true

# Fairness experiment: pipelined keep-alive client A vs single-request client B.
#
# Demonstrates the rule in Puma::Server#process_client:
#   after handling a keep-alive request whose next request is already buffered,
#     - if the thread pool has a *waiting* (idle) thread, the same thread loops
#       and keeps monopolizing the connection (perf optimization, puma #3788);
#     - otherwise the Client object is pushed back onto the pool's @todo queue
#       (@thread_pool << client), so other ready connections get scheduled.
#   Pipelined requests on one connection are still answered in original order.
#
# Usage: ruby -Ilib -Iext/puma_http11 experiments/fairness.rb [max_threads]
require "socket"
require "puma"

MAX_THREADS = Integer(ARGV[0] || 1)
N_A         = 6        # pipelined requests on connection A
DELAY       = 0.15     # simulated app work per request, seconds
HOST        = "127.0.0.1"

t0 = Process.clock_gettime(Process::CLOCK_MONOTONIC)
now = -> { ("%.3f" % (Process.clock_gettime(Process::CLOCK_MONOTONIC) - t0)).rjust(6) }

app = lambda do |env|
  start = Process.clock_gettime(Process::CLOCK_MONOTONIC)
  sleep DELAY
  headers = {
    "x-serve-ms" => ((Process.clock_gettime(Process::CLOCK_MONOTONIC) - start) * 1000).round.to_s,
    "x-thread"   => Thread.current.native_thread_id.to_s,
    "x-req"      => env["HTTP_X_REQ"].to_s
  }
  [200, headers, ["ok"]]
end

server = Puma::Server.new(app, nil, min_threads: 1, max_threads: MAX_THREADS,
                                queue_requests: true, log_writer: Puma::LogWriter.strings)
port = server.add_tcp_listener(HOST, 0).addr[1]
server.run

# minimal HTTP/1.1 response reader, returns [status_line, headers_hash, body]
read_response = lambda do |sock|
  line = sock.gets("\r\n")
  header_lines = []
  while (h = sock.gets("\r\n")) != "\r\n"
    header_lines << h
  end
  headers = header_lines.map(&:strip).each_with_object({}) do |hl, hh|
    k, v = hl.split(":", 2)
    hh[k.downcase] = v.strip
  end
  body = sock.read(headers["content-length"].to_i)
  [line.strip, headers, body]
end

begin
  # ---- client A: one persistent connection, N_A back-to-back (pipelined) requests
  a = TCPSocket.new(HOST, port)
  reqs_a = N_A.times.map do |i|
    "GET /a HTTP/1.1\r\nHost: puma-test\r\nX-Req: A#{i}\r\nContent-Length: 0\r\n\r\n"
  end
  puts "#{now.call}  A sends #{N_A} pipelined requests on one keep-alive connection"
  a.write(reqs_a.join)

  # make sure A's first request has entered the pool before B connects
  sleep DELAY / 2

  # ---- client B: separate connection, single request
  b = TCPSocket.new(HOST, port)
  puts "#{now.call}  B connects and sends its single request"
  b.write("GET /b HTTP/1.1\r\nHost: puma-test\r\nX-Req: B0\r\nConnection: close\r\n\r\n")

  a_thread = Thread.new do
    N_A.times do |i|
      line, headers, = read_response.call(a)
      puts "#{now.call}  A recv resp ##{i}  #{line}  req=#{headers["x-req"]}  " \
           "thread=#{headers["x-thread"]}  serve=#{headers["x-serve-ms"]}ms"
    end
  end

  line, headers, = read_response.call(b)
  puts "#{now.call}  B recv resp       #{line}  req=#{headers["x-req"]}  " \
       "thread=#{headers["x-thread"]}  serve=#{headers["x-serve-ms"]}ms"
  b.close

  a_thread.join
  a.close
ensure
  server.stop(true)
end
