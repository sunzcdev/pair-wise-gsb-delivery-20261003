# frozen_string_literal: true

# Demonstrates how the :push_promise plugin matches HTTP/2 pushed streams to
# explicit requests queued on the session.
#
# The local HTTP/2 server below answers "GET /parent" with a response plus
# three push promises:
#
#   1. "/child?version=1"                        -> exactly matches a request
#      the client queued, so the pushed stream is handed over to that waiter;
#   2. "/child?version=2" + "accept: application/json" -> the client queued
#      the same path, but with a different "accept" header, i.e. it asked for
#      another representation, so the push cannot be delivered to it and is
#      refused;
#   3. "/unrequested"                            -> no waiter at all, refused.
#
# Two scenarios are run:
#
#   * "pending":   requests are sent one at a time (plugin default), so the
#                  matching request is still waiting in the pending queue when
#                  the push promise arrives;
#   * "in-flight": all requests are sent upfront (max_concurrent_requests:
#                  100), so the matching request is already on the wire when
#                  the push promise arrives; the session hands it over to the
#                  pushed stream and releases the stream it was sent on.
#
# Run with: ruby examples/push_promise_matching.rb
# (set HTTPX_DEBUG=2 to also see the per-frame client log, incl. stream ids)

require "webrick"
$LOAD_PATH.unshift(File.expand_path("../lib", __dir__))
require "httpx"
require_relative "../test/support/servlets/test"

$stdout.sync = true

# HTTP/2 server which pushes resources when "/parent" is requested, and
# answers "/child?..." requests slowly (so that, in the in-flight scenario,
# the push promise reaches the client before the plain response does).
class PushMatchServer < TestHTTP2Server
  using HTTPX::URIExtensions

  def initialize(**kw)
    super
    @t0 = Process.clock_gettime(Process::CLOCK_MONOTONIC)
    @write_mutex = Mutex.new
    # connect via the [::1] IP literal: the test listener binds to "::", and
    # resolving "localhost" via DNS is not always available (certificate
    # verification is disabled client-side anyway).
    @origin = @origin.sub("localhost", "[::1]")
  end

  private

  def log_line(msg)
    print format("%<t>7.3f SERVER: %<m>s\n", t: Process.clock_gettime(Process::CLOCK_MONOTONIC) - @t0, m: msg)
  end

  def flush_bytes(sock, bytes)
    @write_mutex.synchronize { super }
  end

  def handle_stream(_sock, conn, stream)
    headers = {}
    stream.on(:headers) { |h| h.each { |k, v| headers[k] = v } }
    stream.on(:half_close) { handle(conn, stream, headers) }
  end

  def handle(conn, stream, headers)
    path = headers[":path"]

    case path
    when "/parent"
      handle_parent(conn, stream)
    when %r{^/child}
      log_line("stream #{stream.id}: GET #{path} (plain response deferred)")
      stream.on(:close) do |error|
        log_line("stream #{stream.id}: plain stream for #{path} was released#{error ? " (#{error})" : ""}")
      end
      # defer the plain response, so the push promise for the same resource
      # reaches the client first in the in-flight scenario.
      Thread.new do
        sleep 0.5
        respond(stream, "PLAIN #{path}", "0")
        log_line("stream #{stream.id}: plain response for #{path} delivered")
      rescue StandardError => e
        log_line("stream #{stream.id}: plain response for #{path} undeliverable (#{e.class}: #{e.message})")
      end
    end
  end

  def handle_parent(conn, stream)
    log_line("stream #{stream.id}: GET /parent")

    stream.headers({
                     ":status" => "200",
                     "content-length" => "6".b,
                     "content-type" => "text/plain",
                   }, end_stream: false)

    # the http-2 gem allocates server-initiated (push) stream ids from a
    # counter which must stay above the highest stream id seen so far, so
    # make room for the push streams above the client-initiated ones (a
    # real server allocates push ids independently of client streams).
    last_stream_id = conn.instance_variable_get(:@last_stream_id)
    if last_stream_id && conn.instance_variable_get(:@stream_id) <= last_stream_id
      next_stream_id = last_stream_id + 1
      next_stream_id += 1 if next_stream_id.odd?
      conn.instance_variable_set(:@stream_id, next_stream_id)
    end

    authority = URI(origin).authority
    promised = []

    [
      { ":path" => "/child?version=1" },
      { ":path" => "/child?version=2", "accept" => "application/json" },
      { ":path" => "/unrequested" },
    ].each do |extra|
      head = { ":method" => "GET", ":scheme" => "https", ":authority" => authority }.merge(extra)

      stream.promise(head) do |push|
        extra_headers = head.reject { |k, _| k.start_with?(":") }
        log_line("stream #{push.id}: promised #{head[':path']}" \
                 "#{extra_headers.empty? ? "" : " with #{extra_headers}"}")
        push.on(:close) do |error|
          log_line("stream #{push.id}: push for #{head[':path']} released#{error ? " (#{error})" : ""}")
        end
        promised << [push, head[":path"]]
      end
    end

    stream.data("PARENT", end_stream: true)

    # deliver the pushed responses with a small delay, so that pushes the
    # client cannot deliver are released (RST_STREAM) while their response
    # is still pending, which makes the release observable here.
    Thread.new do
      sleep 0.3

      promised.each do |push, path|
        respond(push, "PUSHED #{path}", "1")
      rescue StandardError => e
        log_line("stream #{push.id}: pushed response for #{path} undeliverable (#{e.class}: #{e.message})")
      end
    end
  end

  def respond(stream, body, pushed)
    stream.headers({
                     ":status" => "200",
                     "content-type" => "text/plain",
                     "content-length" => body.bytesize.to_s,
                     "x-http2-push" => pushed,
                   }, end_stream: false)
    stream.data(body, end_stream: true)
  end
end

def report(label, response)
  if response.is_a?(HTTPX::ErrorResponse)
    return format("  %-28<l>s -> ERROR %<e>s\n", l: label, e: response.error)
  end

  format(
    "  %-28<l>s -> status=%<s>s pushed?=%<p>-5s x-http2-push=%<h>-5s body=%<b>s\n",
    l: label,
    s: response.status,
    p: response.pushed?,
    h: response.headers["x-http2-push"],
    b: response.to_s
  )
end

def with_server
  server = PushMatchServer.new
  th = Thread.new { server.start }
  yield server.origin
ensure
  sleep 0.7 # let deferred server-side work finish and log
  server.shutdown
  th.join(3) || th.kill
end

def scenario_pending(origin)
  puts "=== scenario 1: matching request still waiting in the pending queue ==="
  session = HTTPX.plugin(:push_promise).with(ssl: { verify_mode: OpenSSL::SSL::VERIFY_NONE })
  parent, child1, child2 = session.get("#{origin}/parent",
                                       "#{origin}/child?version=1",
                                       "#{origin}/child?version=2")
  puts report("GET /parent", parent)
  puts report("GET /child?version=1", child1)
  puts report("GET /child?version=2", child2)
  puts
end

def scenario_in_flight(origin)
  puts "=== scenario 2: matching request already sent on the wire ==="
  session = HTTPX.plugin(:push_promise).with(max_concurrent_requests: 100,
                                             ssl: { verify_mode: OpenSSL::SSL::VERIFY_NONE })
  child1, parent = session.get("#{origin}/child?version=1", "#{origin}/parent")
  puts report("GET /child?version=1", child1)
  puts report("GET /parent", parent)
  puts
end

with_server do |origin|
  scenario_pending(origin)
  scenario_in_flight(origin)
end
