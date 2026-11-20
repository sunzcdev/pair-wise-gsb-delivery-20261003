# frozen_string_literal: true

module Requests
  module Plugins
    module ServerSentEvents
      def test_plugin_server_sent_events
        session = HTTPX.plugin(:server_sent_events).with(ssl: { verify_hostname: false })

        start_test_servlet(SSE, tls: tls?, messages: [{ data: "test" }]) do |server|
          uri = "#{server.origin}/"

          no_sse_response = session.get(uri)
          sse_response = session.get(uri, event_stream: true)

          assert sse_response.respond_to?(:headers) # test respond_to_missing?

          no_sse_headers = no_sse_response.headers
          no_sse_headers.delete("date")
          sse_headers = sse_response.headers
          sse_headers.delete("date")

          verify_header(no_sse_headers, "content-type", "text/plain")
          verify_header(sse_headers, "content-type", "text/event-stream")
          verify_header(sse_headers, "cache-control", "no-cache")
          verify_no_header(sse_headers, "content-length")
        end
      end

      def test_plugin_server_sent_events_each_message
        messages = [
          { event: "test", data: "test1", id: 1 },
          { event: "test", data: "test2", id: 2 },
          { comment: "this is a comment" },
          { event: "test", data: "test3" },
        ]
        session = HTTPX.plugin(:server_sent_events).with(ssl: { verify_hostname: false })
        start_test_servlet(SSE, tls: tls?, messages: messages) do |server|
          uri = "#{server.origin}/"

          response = session.get(uri, event_stream: true)
          messages = response.each_message.to_a
          assert messages.size == 3, "all the messages should have been yielded"
          assert(messages.all? { |m| m.event == "test" })
          assert messages[0].id == "1"
          assert messages[0].data == "test1"
          assert messages[1].id == "2"
          assert messages[1].data == "test2"
          assert messages[2].id.nil?
          assert messages[2].data == "test3"
        end
      end

      def test_plugin_server_sent_events_multiple_datas_concat_single_message
        messages = [
          { data: "test1" },
          { data: %w[test2 test3] },
        ]
        session = HTTPX.plugin(:server_sent_events).with(ssl: { verify_hostname: false })
        start_test_servlet(SSE, tls: tls?, messages: messages) do |server|
          uri = "#{server.origin}/"

          response = session.get(uri, event_stream: true)
          messages = response.each_message.to_a
          assert messages.size == 2, "all the messages should have been yielded"
          assert messages[0].data == "test1"
          assert messages[1].data == "test2\ntest3"
        end
      end

      def test_plugin_server_sent_events_chunked_across_byte_boundaries
        messages = [
          { event: "test", data: "tést1", id: 1 },
          { data: %w[tést2 tést3] },
          { id: 2, data: "tést4" },
        ]
        reference = nil

        [nil, 1, 2, 3, 5, 7].each do |chunk_size|
          session = HTTPX.plugin(:server_sent_events).with(ssl: { verify_hostname: false })
          start_test_servlet(SSE, tls: tls?, messages: messages, chunk_size: chunk_size) do |server|
            uri = "#{server.origin}/"

            response = session.get(uri, event_stream: true)
            received = response.each_message.to_a
            parsed = received.map { |m| [m.id, m.event, m.data] }
            last_id = response.last_event_id

            if reference
              assert parsed == reference[0], "messages should not depend on the read chunking (chunk_size: #{chunk_size})"
              assert last_id == reference[1], "last event id should not depend on the read chunking (chunk_size: #{chunk_size})"
            else
              reference = [parsed, last_id]
            end

            assert received.size == 3
            assert received[0].data == "tést1"
            assert received[0].id == "1"
            assert received[1].data == "tést2\ntést3"
            assert received[2].data == "tést4"
            assert received[2].id == "2"
            assert last_id == "2"
          end
        end
      end

      def test_plugin_server_sent_events_empty_id_resets_recovery_position
        messages = [
          { data: "test1", id: 1 },
          { data: "test2", id: 2 },
          { data: "test3", id: "" },
          { data: "test4" },
        ]
        session = HTTPX.plugin(:server_sent_events).with(ssl: { verify_hostname: false })
        start_test_servlet(SSE, tls: tls?, messages: messages) do |server|
          uri = "#{server.origin}/"

          response = session.get(uri, event_stream: true)
          received = response.each_message.to_a

          # an event with an empty "id" resets the recovery position, which is then
          # reported as an empty string (as opposed to "no id seen yet", which is nil).
          assert received.map(&:id) == ["1", "2", "", nil]
          assert response.last_event_id == ""

          # an event without an "id" field leaves the position untouched.
          assert received[-1].data == "test4"
        end
      end

      def test_plugin_server_sent_events_last_event_id_option
        session = HTTPX.plugin(:server_sent_events).with(ssl: { verify_hostname: false })

        start_test_servlet(SSE, tls: tls?, messages: [{ data: "test" }]) do |server|
          uri = "#{server.origin}/"

          request = session.build_request("GET", uri, event_stream: true)
          verify_no_header(request.headers, "last-event-id")

          request = session.build_request("GET", uri, event_stream: true, last_event_id: "42")
          verify_header(request.headers, "last-event-id", "42")

          # an empty id resets the position, so it must not be sent.
          request = session.build_request("GET", uri, event_stream: true, last_event_id: "")
          verify_no_header(request.headers, "last-event-id")

          # the option defaults do not leak between requests built from the same session.
          request = session.build_request("GET", uri, event_stream: true)
          verify_no_header(request.headers, "last-event-id")
        end
      end

      def test_plugin_server_sent_events_dropped_mid_event_does_not_advance_last_event_id
        messages = [
          { data: "test1", id: 1 },
          { data: "test2", id: 2 },
        ]
        session = HTTPX.plugin(:server_sent_events).with(ssl: { verify_hostname: false })
        # the stream ends in the middle of an event, and in the middle of a character.
        start_test_servlet(SSE, tls: tls?, messages: messages, trailing: +"data: hal\xC3".b) do |server|
          uri = "#{server.origin}/"

          response = session.get(uri, event_stream: true)
          received = response.each_message.to_a

          # the incomplete event must neither be yielded, nor move the recovery position.
          assert received.map(&:id) == %w[1 2]
          assert received.map(&:data) == %w[test1 test2]
          assert response.last_event_id == "2"
        end
      end

      def test_plugin_server_sent_events_retries_last_used_id
        return unless tls?

        messages = [
          { data: "test1", id: 1 },
          { data: "test2", id: 2 },
          { data: "test3", id: 3 },
        ]
        session = HTTPX.plugin(RequestInspector)
                       .plugin(:retries)
                       .plugin(:server_sent_events)
                       .with(ssl: { verify_hostname: false })
        start_test_servlet(SSE, tls: tls?, messages: messages, close_after: 2) do |server|
          uri = "#{server.origin}/"

          response = session.get(uri, event_stream: true)
          messages = response.each_message.to_a
          total_requests = session.total_requests

          # assert that there was an actual retry
          assert total_requests.size == 2
          first_request, retry_request = total_requests
          verify_no_header(first_request.headers, "last-event-id")
          verify_header(retry_request.headers, "last-event-id", "2")

          # assert that the messages before the retry aren't repeated
          assert messages.size == 3, "all the messages should have been yielded"
          assert messages[0].id == "1"
          assert messages[0].data == "test1"
          assert messages[1].id == "2"
          assert messages[1].data == "test2"
          assert messages[2].id == "3"
          assert messages[2].data == "test3"
        end
      end

      def test_plugin_server_sent_events_retries_last_used_id_retry_after
        return unless tls?

        messages = [
          { data: "test1", id: 1, retry: 2000 },
          { data: "test2", id: 2, retry: 2000 },
          { data: "test3", id: 3, retry: 2000 },
        ]
        session = HTTPX.plugin(RequestInspector).plugin(:retries).plugin(:server_sent_events).with(ssl: { verify_hostname: false })
        start_test_servlet(SSE, tls: tls?, messages: messages, close_after: 2) do |server|
          uri = "#{server.origin}/"

          before_time = Process.clock_gettime(Process::CLOCK_MONOTONIC, :second)
          response = session.get(uri, event_stream: true)
          messages = response.each_message.to_a
          after_time = Process.clock_gettime(Process::CLOCK_MONOTONIC, :second)
          total_time = after_time - before_time
          total_requests = session.total_requests

          # assert that there was an actual retry
          assert total_requests.size == 2
          first_request, retry_request = total_requests
          verify_no_header(first_request.headers, "last-event-id")
          verify_header(retry_request.headers, "last-event-id", "2")

          verify_execution_delta(2, total_time, 1)

          # assert that the messages before the retry aren't repeated
          assert messages.size == 3, "all the messages should have been yielded"
          assert messages[0].id == "1"
          assert messages[0].data == "test1"
          assert messages[1].id == "2"
          assert messages[1].data == "test2"
          assert messages[2].data == "test3"
        end
      end
    end
  end
end
