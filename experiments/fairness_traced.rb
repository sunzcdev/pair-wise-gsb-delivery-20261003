require "socket"
require "puma"
require "puma/thread_pool"
STDOUT.sync = true

t0 = Process.clock_gettime(Process::CLOCK_MONOTONIC)
NOW = -> { ("%.3f" % (Process.clock_gettime(Process::CLOCK_MONOTONIC) - t0)).rjust(6) }

lw = Puma::LogWriter.strings
Puma::ThreadPool.class_eval do
  alias_method :orig_enqueue, :<<
  def <<(work)
    label = work.respond_to?(:requests_served) ? "keep-alive client(#{work.requests_served} served)" : "new client"
    puts "#{NOW.call}  enqueue #{label.ljust(30)} waiting_before=#{waiting}"
    orig_enqueue(work)
  end
end

app = lambda { |env| sleep 0.12; [200, {"x-req" => env["HTTP_X_REQ"]}, ["ok"]] }
server = Puma::Server.new(app, nil, min_threads: 1, max_threads: 1,
                                queue_requests: true, log_writer: lw)
port = server.add_tcp_listener("127.0.0.1", 0).addr[1]
server.run

watchdog = Thread.new do
  sleep 8
  puts "=== WATCHDOG TIMEOUT ==="
  Thread.list.each { |t| puts "thread #{t.name} alive=#{t.alive?}" }
  puts "--- server log stdout: #{lw.instance_variable_get(:@stdout)&.string}"
  puts "--- server log stderr: #{lw.instance_variable_get(:@stderr)&.string}"
  exit! 3
end

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
  puts "#{NOW.call}  A pipelined 4 requests"
  sleep 0.06
  b = TCPSocket.new("127.0.0.1", port)
  b.write("GET /b HTTP/1.1\r\nHost: t\r\nX-Req: B0\r\nConnection: close\r\nContent-Length: 0\r\n\r\n")
  puts "#{NOW.call}  B sent single request"
  th = Thread.new { 4.times { |i| read_resp.call(a, "A", i) } }
  read_resp.call(b, "B", 0)
  th.join
ensure
  watchdog.kill
  a&.close
  b&.close
  server.stop(true)
end
