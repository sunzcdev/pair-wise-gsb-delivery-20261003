# frozen_string_literal: true

# Released under the MIT License.
# Copyright, 2026, by Samuel Williams.

require "async/http/protocol/http2"
require "sus/fixtures/async/scheduler_context"

require "async/promise"
require "socket"

describe Async::HTTP::Protocol::HTTP2 do
  include Sus::Fixtures::Async::SchedulerContext

  # A high-reuse HTTP/2 connection keeps processing requests while the application may hold a
  # request object for a long time before reading its headers or body. Two streams are opened on
  # one connection: the application pauses after taking the first request headers, the second
  # request completes, and trailers are only then delivered on the first stream. Each stream
  # releases its protocol-side header buffer at the safe point, so both application views remain
  # stable and each trailer is read against the stream it belongs to.
  with "retained requests and received trailers" do
    let(:sockets) {Socket.pair(Socket::PF_UNIX, Socket::SOCK_STREAM)}

    # Resolved once the application has taken the first request's initial header view.
    let(:first_request_headers_seen) {Async::Promise.new}

    # Resolved by the test to release the paused application task for the first request.
    let(:release_first_request) {Async::Promise.new}

    let(:server_requests) {[]}
    let(:server_views) {Async::Queue.new}

    let(:app) do
      requests = server_requests
      seen = first_request_headers_seen
      release = release_first_request
      views = server_views

      Protocol::HTTP::Middleware.for do |request|
        requests << request
        views.enqueue(request.headers)

        if requests.size == 1
          # The application has taken a stable view of the initial headers and now pauses before
          # touching the body. The second request completes while this one is parked.
          seen.resolve(true)
          release.wait
        end

        # Drain the request body (and, for the first request, its trailers) before responding.
        request.finish

        Protocol::HTTP::Response[200, {}, [request.headers["x-request"].first]]
      end
    end

    def after(error = nil)
      sockets.each {|socket| socket.close unless socket.closed?}

      super
    end

    it "keeps stable application views while buffering trailers per stream" do
      server_task = Async(transient: true) do
        server = subject.server(IO::Stream(sockets.last))

        server.each do |request|
          app.call(request)
        end
      ensure
        server&.close
      end

      client = subject.client(IO::Stream(sockets.first))

      make_request = lambda do |path, headers, body|
        request = Protocol::HTTP::Request.new("http", "localhost", "POST", path, nil, headers, body)
        client.call(request)
      end

      # First stream: headers + an open request body whose trailers arrive later.
      first_headers = Protocol::HTTP::Headers.new
      first_headers.add("trailer", "etag")
      first_headers.add("x-request", "first")
      first_body = Async::HTTP::Body::Writable.new

      # The client writes the first request headers (and starts the body output task), then waits
      # for the response. The request body itself is kept open until the end of the test.
      first_exchange = Async do
        make_request.call("/first", first_headers, first_body)
      end

      # Wait until the server application has taken the first request headers and parked.
      first_view = server_views.dequeue
      first_request_headers_seen.wait

      # Second stream completes fully, including its own trailer, on the same connection.
      second_headers = Protocol::HTTP::Headers.new
      second_headers.add("trailer", "etag")
      second_headers.add("x-request", "second")
      second_body = Async::HTTP::Body::Writable.new

      second_exchange = Async do
        make_request.call("/second", second_headers, second_body)
      end

      second_body.write("second body")
      second_headers.add("etag", "second-etag")
      second_body.close_write

      second_response = second_exchange.wait
      expect(second_response.read).to be == "second"
      second_response.finish

      second_view = server_views.dequeue

      # Now deliver the first stream trailers, after the second stream has fully completed.
      first_body.write("first body")
      first_headers.add("etag", "first-etag")
      first_body.close_write

      # Release the parked application task for the first request so it can drain body + trailers.
      release_first_request.resolve(true)

      first_response = first_exchange.wait
      expect(first_response.read).to be == "first"
      first_response.finish

      # Both requests were served on the same multiplexed connection.
      expect(server_requests.size).to be == 2

      # Each application view keeps its own initial header section, unaffected by trailers that
      # arrived later or by activity on the other stream.
      expect(first_view["x-request"]).to be == ["first"]
      expect(second_view["x-request"]).to be == ["second"]

      # Trailers are distinct from the initial header section.
      expect(first_view).to be(:trailer?)
      expect(second_view).to be(:trailer?)

      expect(first_view.header.to_a).to be == [
        ["trailer", "etag"],
        ["x-request", "first"],
      ]
      expect(second_view.header.to_a).to be == [
        ["trailer", "etag"],
        ["x-request", "second"],
      ]

      # Each trailer is read against the stream it belongs to.
      expect(first_view.trailer.to_a).to be == [["etag", "first-etag"]]
      expect(second_view.trailer.to_a).to be == [["etag", "second-etag"]]
    ensure
      first_body&.close
      second_body&.close
      client&.close
      server_task&.stop
    end

    it "does not create a trailer section for an empty trailing frame" do
      captured_view = Async::Queue.new

      server_task = Async(transient: true) do
        server = subject.server(IO::Stream(sockets.last))

        server.each do |request|
          request.finish
          captured_view.enqueue(request.headers)

          Protocol::HTTP::Response[200, [], "empty trailer"]
        end
      ensure
        server&.close
      end

      client = subject.client(IO::Stream(sockets.first))

      response = client.create_response

      # Keep the request body open so the client does not end the stream itself:
      body = Async::HTTP::Body::Writable.new
      body.write("Hello")

      request = Protocol::HTTP::Request.new("http", "localhost", "POST", "/", nil, Protocol::HTTP::Headers.new, body)
      response.send_request(request)

      # End the stream with a trailing HEADERS frame that carries no fields instead.
      response.stream.send_headers([], Protocol::HTTP2::END_STREAM)

      client.read_response(response)
      expect(response.read).to be == "empty trailer"

      view = captured_view.dequeue

      expect(view).not.to be(:trailer?)
      expect(view.trailer.to_a).to be(:empty?)
    ensure
      body&.close
      client&.close
      server_task&.stop
    end
  end
end