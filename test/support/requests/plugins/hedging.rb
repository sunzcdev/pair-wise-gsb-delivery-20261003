# frozen_string_literal: true

require_relative "../../servlets/hedge_status"

module Requests
  module Plugins
    module Hedging
      module KeepHedgeConnections
        module InstanceMethods
          def connections_and_hedge
            connections.flat_map do |conn|
              [conn, conn.deleted_hedge_connection]
            end.compact.uniq
          end
        end

        module ConnectionMethods
          attr_reader :deleted_hedge_connection

          def hedge_connection=(conn)
            return super unless conn.nil? # rubocop:disable Lint/ReturnInVoidContext

            @deleted_hedge_connection = @hedge_connection

            super
          end
        end
      end

      def test_hedging_multiple_connections
        uri = URI(build_uri("/get"))
        HTTPX.plugin(SessionWithPool)
             .plugin(:hedging)
             .plugin(KeepHedgeConnections).wrap do |http|
          response = http.get(uri)

          verify_status(response, 200)

          connections = http.connections_and_hedge

          assert connections.size == 2
          conn1, conn2 = connections

          # check that both connections do not share the IO object
          assert !conn1.io.nil?
          assert !conn2.io.nil?
          assert conn1.io != conn2.io, "there should be two different sockets"
          # check that both connections are the same in everything else
          assert conn1.origins == conn2.origins
          assert conn1.addresses == conn2.addresses
          assert conn1.match?(uri, conn1.options), "connections should be the same and matchable"
          assert conn2.match?(uri, conn2.options), "connections should be the same and matchable"
        end
      end

      def test_hedging_delay_before_new_connection
        uri = URI(build_uri("/get"))
        delay_uri = build_uri("/delay/6")

        http = HTTPX.plugin(SessionWithPool)
                    .plugin(:hedging, hedge_after: 4)
                    .plugin(KeepHedgeConnections)

        response = http.get(uri)

        verify_status(response, 200)

        connections = http.connections_and_hedge
        assert connections.size == 1

        http.connections.clear
        http.pool.connections.clear

        response = http.get(delay_uri)
        verify_status(response, 200)

        connections = http.connections_and_hedge

        assert connections.size == 2
        conn1, conn2 = connections

        # check that both connections do not share the IO object
        assert !conn1.io.nil?
        assert !conn2.io.nil?
        assert conn1.io != conn2.io, "there should be two different sockets"
        # check that both connections are the same in everything else
        assert conn1.origins == conn2.origins
        assert conn1.addresses == conn2.addresses
        assert conn1.match?(uri, conn1.options), "connections should be the same and matchable"
        assert conn2.match?(uri, conn2.options), "connections should be the same and matchable"
      end

      def test_hedging_persistent_do_not_retain_hedge_connections
        uri = URI(build_uri("/get"))

        begin
          http = HTTPX.plugin(SessionWithPool)
                      .plugin(:persistent)
                      .plugin(:hedging)
                      .plugin(KeepHedgeConnections)

          response = http.get(uri)

          verify_status(response, 200)

          pool = http.pool
          connections = http.connections_and_hedge

          assert connections.size == 2
          conn1, conn2 = connections
          hedge_conn, main_conn = connections.partition(&:is_hedge_connection).map(&:first)

          # check that both connections do not share the IO object
          assert !conn1.io.nil?
          assert !conn2.io.nil?
          assert conn1.io != conn2.io, "there should be two different sockets"
          # check that both connections are the same in everything else
          assert conn1.origins == conn2.origins
          assert conn1.addresses == conn2.addresses
          assert conn1.match?(uri, conn1.options), "connections should be the same and matchable"
          assert conn2.match?(uri, conn2.options), "connections should be the same and matchable"

          # check that only one of them is in the pool
          pool_connections = pool.connections
          assert pool_connections.size == 1
          assert pool_connections == [main_conn]
          assert main_conn.state == (scheme == "https://" ? :inactive : :closed)
          assert_includes(%i[idle closed], hedge_conn.state)
        ensure
          http.close
        end
      end

      def test_hedging_accept_response_rejected_first_attempt_keeps_other_running
        start_test_servlet(HedgeStatusServer, statuses: [500, 200]) do |server|
          response = HTTPX.plugin(:hedging,
                                   accept_response: ->(res) { !res.is_a?(HTTPX::ErrorResponse) && res.status < 500 })
                           .get(server.origin.to_s)

          verify_status(response, 200)

          # both attempts must have been sent while racing each other.
          assert server.request_count[0] == 2, "expected both hedge attempts to be sent (was #{server.request_count[0]})"
        end
      end

      def test_hedging_accept_response_first_acceptable_wins
        start_test_servlet(HedgeStatusServer, statuses: [200, 500]) do |server|
          response = HTTPX.plugin(:hedging,
                                   accept_response: ->(res) { !res.is_a?(HTTPX::ErrorResponse) && res.status < 500 })
                           .get(server.origin.to_s)

          verify_status(response, 200)
        end
      end

      def test_hedging_accept_response_all_rejected_returns_traceable_hedging_error
        start_test_servlet(HedgeStatusServer, statuses: [500, 503]) do |server|
          response = HTTPX.plugin(:hedging,
                                   accept_response: ->(res) { !res.is_a?(HTTPX::ErrorResponse) && res.status < 500 })
                           .get(server.origin.to_s)

          verify_error_response(response, HTTPX::HedgingError)

          error = response.error

          # the failure must be traceable back to every single attempt.
          assert error.responses.size == 2, "expected 2 attempt responses (was #{error.responses.size})"
          assert_equal [500, 503], error.responses.map(&:status)
          assert error.message.include?("HTTP 500")
          assert error.message.include?("HTTP 503")
          assert server.request_count[0] == 2, "expected both hedge attempts to be sent (was #{server.request_count[0]})"
        end
      end

      def test_hedging_accept_response_transport_errors_are_aggregated
        # a server which accepts connections only to close them right away, so that
        # both hedge attempts fail at the connection stage (after resolution, when
        # the hedge pair is already established).
        server = TCPServer.new("127.0.0.1", 0)
        port = server.addr[1]
        thread = Thread.new do
          loop do
            begin
              client = server.accept
              client.close
            rescue IOError
              break
            end
          end
        end

        begin
          response = HTTPX.plugin(:hedging,
                                   accept_response: ->(res) { !res.is_a?(HTTPX::ErrorResponse) })
                           .with(timeout: { connect_timeout: 3, request_timeout: 5 })
                           .get("http://127.0.0.1:#{port}/")

          verify_error_response(response, HTTPX::HedgingError)

          responses = response.error.responses

          assert responses.size == 2, "expected 2 attempt errors (was #{responses.size})"
          assert(responses.all? { |res| res.is_a?(HTTPX::ErrorResponse) })
        ensure
          server.close
          thread.join
        end
      end

      def test_hedging_without_accept_response_keeps_default_first_wins_behaviour
        start_test_servlet(HedgeStatusServer, statuses: [500, 200]) do |server|
          response = HTTPX.plugin(:hedging).get(server.origin.to_s)

          # without an acceptance policy, the first completed response wins as before.
          verify_status(response, 500)
        end
      end

      def test_hedging_accept_response_must_be_callable
        assert_raises(TypeError) { HTTPX.plugin(:hedging, accept_response: "not-callable") }
      end

      def test_hedging_all_rejected_connections_are_reusable
        start_test_servlet(HedgeStatusServer, statuses: [500, 500]) do |server|
          http = HTTPX.plugin(:persistent)
                      .plugin(SessionWithPool)
                      .plugin(:hedging, accept_response: ->(res) { res.status < 500 })

          begin
            failing = http.get(server.origin.to_s)
            verify_error_response(failing, HTTPX::HedgingError)

            # a subsequent successful request must still be served, i.e. healthy
            # connections weren't permanently discarded by the all-failed hedge.
            server.mount_proc("/ok") { |_req, res| res.status = 200; res.body = "ok" }
            ok = http.get("#{server.origin}/ok")
            verify_status(ok, 200)
          ensure
            http.close
          end
        end
      end
    end
  end
end
