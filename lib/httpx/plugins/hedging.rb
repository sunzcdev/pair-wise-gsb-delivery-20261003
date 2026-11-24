# frozen_string_literal: true

module HTTPX
  # Error wrapping the per-attempt failures of a hedged request, when none of the
  # attempts produced an acceptable response (i.e. the +:accept_response+ callback
  # rejected every attempt, or every attempt ended in an error response).
  #
  # It exposes the ordered list of +HTTPX::Response+ (or +HTTPX::ErrorResponse+)
  # objects collected for each attempt through +#responses+, so that the failure
  # of every single attempt can be traced back and inspected.
  class HedgingError < Error
    # the ordered per-attempt responses (HTTPX::Response or HTTPX::ErrorResponse)
    attr_reader :responses

    def initialize(responses)
      @responses = responses

      super(
        "all hedged request attempts failed (#{responses.size}): " \
        "#{responses.map { |response| describe(response) }.join('; ')}"
      )
    end

    # closes the response body handles (if any) held by the failed attempts.
    def close
      @responses.each do |response|
        response.close if response.respond_to?(:close)
      end
    end

    private

    def describe(response)
      case response
      when ErrorResponse
        "#{response.error.class}: #{response.error.message}"
      else
        "HTTP #{response.status}"
      end
    end
  end

  module Plugins
    #
    # This plugin adds support for request hedging, i.e. a latency-mitigation strategy that involves
    # sending requests through multiple connections and returning the fastest response.
    #
    # By default, the first completed response wins, regardless of whether it is successful.
    # When the +:accept_response+ option is set, an attempt response is only accepted if the
    # callback returns a truthy value; rejected attempts keep running, and the first remaining
    # acceptable response wins instead. If no attempt is acceptable (or every attempt errors),
    # an +HTTPX::HedgingError+ wrapping every per-attempt response is returned.
    #
    # https://gitlab.com/os85/httpx/wikis/Hedging#hedging
    #
    module Hedging
      # :hedge_after:: seconds after which, if the request didn't complete yet, it'll be sent on a different connection
      # :accept_response:: callable (i.e. <tt>->(response) { ... }</tt>) which decides whether an attempt response can
      #                    be accepted as the hedged request response. When not given, the first completed response wins.
      module OptionsMethods
        private

        def option_hedge_after(value)
          if value.respond_to?(:call)
            value1 = value
            value1 = value1.method(:call) unless value1.respond_to?(:arity)

            # allow ->(*) arity as well, which is < 0
            raise TypeError, "`:hedge_after` proc has invalid number of parameters" unless value1.arity.negative? || value1.arity.between?(
              1, 2
            )

          else
            value = Float(value)
            raise TypeError, "`:hedge_after` must be positive" unless value.positive?
          end

          value
        end

        def option_accept_response(value)
          raise TypeError, ":accept_response must be called with the response" unless value.respond_to?(:call)

          value
        end
      end

      module InstanceMethods
        def select_connection(connection, selector)
          connection.try_yield_hedge_connection do |hedge_conn|
            super.tap do
              select_connection(hedge_conn, selector) if hedge_conn
            end
          end
        end

        private

        def do_request(request, connection, selector)
          super.tap do
            # forego hedging if the connection is already open, or if
            # the connection is scheduled for DNS resolution, as pending requests
            # will be hedged afterwards.
            next if connection.open? || !connection.addresses?

            hedge_request!(request, connection, selector)
          end
        end

        # creates the hedge connection before a connection initiates the connection handshake.
        # This excludes connections which are already open.
        def on_resolver_connection(connection, selector)
          super.tap do
            next if connection.open? || connection.hedge_connection

            connection.pending.each do |request|
              hedge_request!(request, connection, selector)
            end

            on_resolver_connection(connection.hedge_connection, selector) if connection.hedge_connection
          end
        end

        def hedge_connection(connection)
          connection.hedge_connection || begin
            new_connection = connection.dup
            new_connection.log { "cloning hedge connection of connection##{connection.object_id}..." }
            connection.hedge_connection = new_connection
            new_connection
          end
        end

        def hedge_request!(request, connection, selector)
          hedge_after = when_to_hedge(request)

          if hedge_after&.positive?
            # hedge after interval
            cb = selector.after(hedge_after) do
              next if request.response
              # the request may have been hedged in the meantime (i.e. through a
              # sibling connection being resolved), or already settled.
              next if request.hedge_request

              request.hedge_request = hedge_request = request.dup
              hedge_conn = hedge_connection(connection)
              request.remember_hedge_connections(connection, hedge_conn)
              hedge_conn.send(hedge_request)
              select_connection(hedge_conn, selector)
            end

            # forego hedging if request is over
            request.once(:response) do
              cb.cancel
            end
          else
            return if request.hedge_request

            request.hedge_request = hedge_request = request.dup
            hedge_conn = hedge_connection(connection)
            request.remember_hedge_connections(connection, hedge_conn)
            hedge_conn.send(hedge_request)
            # the hedge connection must be registered in the selector, otherwise it
            # is never driven and the hedge request never gets sent.
            select_connection(hedge_conn, selector)
          end
        end

        def when_to_hedge(request)
          hedge_after = request.options.hedge_after

          return unless hedge_after

          hedge_after = hedge_after.call(request) if hedge_after.respond_to?(:call)

          hedge_after
        end

        # hedge connections should not deactivate, instead they should terminate.
        # this ensures that persistent hedge connections aren't treated as persistent,
        # and close instead of deactivating.
        def deactivate(selector, *)
          hedge_connections = selector.each_connection.select(&:is_hedge_connection)

          return super unless hedge_connections.any?

          selector_close(selector, hedge_connections)

          super
        end

        # do not check hedge connections back in
        def can_checkin?(connection)
          super && !connection.is_hedge_connection
        end
      end

      module RequestMethods
        # TODO: would be nice to get rid of this
        attr_reader :connection

        attr_reader :hedge_request

        def initialize(*)
          super
          @is_hedge_request = false
          # root request of the hedge pair (the non-hedge request). It points to
          # itself until the request gets hedged.
          @hedge_main_request = self
          # the connections carrying the main and the hedge attempt. Captured when
          # the hedge pair is established, since request connections get cleared as
          # soon as responses finish.
          @hedge_connections = []
          # ordered per-attempt responses, shared between the main request and its
          # hedge request. It records responses which were not accepted, so that, if
          # every attempt fails to be accepted, they can all be traced back.
          @hedge_attempts = []
        end

        # dupped initialization: a hedge request is its own request, and must not
        # inherit the hedge bookkeeping of the request it was duplicated from.
        def initialize_dup(orig)
          super
          @is_hedge_request = false
          @hedge_request = nil
          @hedge_main_request = self
          @hedge_accepted_response = nil
          @hedge_connections = []
          @hedge_attempts = []
        end

        # records the connection pair carrying the main and the hedge attempt, and
        # shares it with the hedge request so it survives response finalization.
        def remember_hedge_connections(main_connection, hedge_connection)
          @hedge_connections = [main_connection, hedge_connection].compact.uniq
          @hedge_main_request.instance_variable_set(:@hedge_connections, @hedge_connections)
        end

        def hedge_request=(request)
          # unlinking (request is nil): just clear the reference.
          return @hedge_request = nil unless request

          if @hedge_main_request.equal?(self) && !@is_hedge_request
            # self is the main request: establish the bidirectional link with the
            # given hedge request, without relying on a recursive setter.
            @hedge_request = request

            request.instance_variable_set(:@hedge_request, self)
            request.instance_variable_set(:@is_hedge_request, true)
            request.instance_variable_set(:@hedge_main_request, self)
            request.instance_variable_set(:@hedge_attempts, @hedge_attempts)
          else
            # self is a hedge request being (re)linked: keep the reference, but never
            # reassign the main request nor the shared attempts ledger.
            @hedge_request = request
          end
        end

        def response=(response)
          return super unless @hedge_request # rubocop:disable Lint/ReturnInVoidContext

          prev_response = @response

          begin
            super
          ensure
            @response = prev_response if prev_response
          end
        end

        def emit_response(response)
          hedge_request = @hedge_request

          return super unless hedge_request

          # an attempt already produced an acceptable response: any late response
          # (i.e. an HTTP/1 request which could not be cancelled) is ignored.
          return if hedge_accepted?

          if acceptable_hedge_response?(response)
            # the attempt response can be accepted: it becomes the hedged request
            # response, gets propagated to the main request, and the remaining
            # attempt is stopped.
            main_request = main_hedge_request
            main_request.instance_variable_set(:@hedge_accepted_response, response)

            # stop the remaining attempt first: cancels any pending HTTP/2 stream.
            # There's no way to cancel an HTTP/1 request, so that one will run to
            # completion (and its late response will be ignored above).
            hedge_request.emit(:refuse)

            # unlink the hedge pair before settling, so the winning response flows
            # through the base request response lifecycle.
            detach_hedge_requests

            settle_accepted_hedge_response(main_request, response)
          else
            # the response cannot be accepted: keep it on the attempt which carried it
            # and record it for traceability, but do not emit it upstream (so the
            # main request is not settled and its +:response+ callback is preserved).
            # The remaining attempt keeps running; once every attempt reported back
            # without being accepted, the request settles with an aggregated error.
            log(level: 2) { "hedge attempt response rejected, waiting for remaining attempt..." }

            self.response = response unless @response.equal?(response)

            @hedge_attempts << response

            settle_all_hedge_attempts_failed if hedge_requests_settled?
          end
        end

        # whether the given +response+ is the one which was accepted as the hedged
        # response (as opposed to a per-attempt response merely rejected by the
        # +:accept_response+ policy).
        def hedge_accepted_response?(response)
          main_hedge_request.instance_variable_get(:@hedge_accepted_response).equal?(response)
        end

        private

        # whether an acceptable response already won the hedged request.
        def hedge_accepted?
          main_hedge_request.instance_variable_get(:@hedge_accepted_response) != nil
        end

        # returns the root request of the hedge pair (the non-hedge request).
        def main_hedge_request
          @hedge_main_request || self
        end

        # whether the given attempt response can be accepted as the hedged response.
        # without an +:accept_response+ callback, the first completed response wins,
        # preserving the default hedging behaviour.
        def acceptable_hedge_response?(response)
          accept_response = options.accept_response

          return true unless accept_response

          accept_response.call(response) ? true : false
        end

        # whether every attempt (main and hedge) has reported a (non-accepted) response.
        def hedge_requests_settled?
          @hedge_attempts.size >= 2
        end

        # unlinks the main request and its hedge request, so late callbacks become
        # no-ops and objects do not leak.
        def detach_hedge_requests
          main_request = main_hedge_request
          hedge_request = main_request.hedge_request

          return unless hedge_request

          hedge_request.instance_variable_set(:@hedge_request, nil)
          main_request.instance_variable_set(:@hedge_request, nil)
        end

        # builds and emits the terminal error response wrapping every failed attempt,
        # and releases the hedge connections so that healthy ones can be reused.
        def settle_all_hedge_attempts_failed
          responses = @hedge_attempts.dup

          main_request = main_hedge_request

          # use the connections captured when the hedge pair was established, as the
          # per-request connections are cleared once responses finish.
          connections = main_request.instance_variable_get(:@hedge_connections).dup

          detach_hedge_requests

          release_hedge_connections(connections)

          error = HedgingError.new(responses)
          log_exception(error)

          error_response = ErrorResponse.new(main_request, error)
          main_request.response = error_response

          emit_base_response(main_request, error_response)
        end

        # settles the main request with the accepted winning +response+, going
        # through the base request response lifecycle (including the
        # +on_response_arrived+ callback which drives the session selector loop).
        def settle_accepted_hedge_response(main_request, response)
          main_request.response = response

          emit_base_response(main_request, response)
        end

        # invokes the base +HTTPX::Request#emit_response+ on +request+, bypassing
        # the hedge override. This is required because the final response must always
        # be settled through the main request lifecycle (emitting +:response+ and
        # triggering +on_response_arrived+), regardless of which attempt carried it.
        def emit_base_response(request, response)
          Request.instance_method(:emit_response).bind_call(request, response)
        end

        # unlinks the given hedge connections and marks them as reusable. The
        # failures which settled the hedged request may be application-level (i.e.
        # rejections through +:accept_response+), in which case neither connection is
        # inherently unusable; dead connections are still closed through the normal
        # lifecycle. The hedge links may already have been cleared by +Response#finish!+
        # by the time this runs, so the captured connections are marked reusable
        # directly rather than through the (possibly gone) links.
        def release_hedge_connections(connections)
          connections.each do |conn|
            if (hedge_connection = conn.hedge_connection)
              conn.hedge_connection = nil
              hedge_connection.hedge_connection = nil
            end

            conn.is_hedge_connection = false
          end
        end
      end

      module ResponseMethods
        def finish!
          connection = @request.connection

          super

          release_hedge_connection(connection)
        end

        private

        # unlinks the connection from its hedge, when the response it carried
        # settled the hedged request. Healthy connections are marked as reusable,
        # so that they can be checked back into the pool instead of being discarded.
        def release_hedge_connection(connection)
          return unless connection && (hedge_connection = connection.hedge_connection)

          # disconnect connection with its hedge, so that objects do not leak after
          # they're not needed.
          connection.hedge_connection = hedge_connection.hedge_connection = nil

          # remarking itself as the main connection, so that it's not discarded when
          # going back to the pool.
          connection.is_hedge_connection = false
          hedge_connection.is_hedge_connection = true
        end
      end

      module ConnectionMethods
        attr_reader :hedge_connection

        attr_accessor :is_hedge_connection

        def initialize(*)
          super
          @is_hedge_connection = false
        end

        def addresses=(addrs)
          try_yield_hedge_connection do |hedge_conn|
            super.tap do
              hedge_conn.addresses = addrs if hedge_conn
            end
          end
        end

        def hedge_connection=(connection)
          @hedge_connection = connection

          return unless connection

          @is_hedge_connection = !connection.hedge_connection.nil?

          return if @is_hedge_connection

          connection.hedge_connection = self
        end

        def try_yield_hedge_connection
          return yield(nil) unless (hedge_conn = @hedge_connection)

          @hedge_connection = nil

          begin
            hedge_conn.try_yield_hedge_connection do
              yield(hedge_conn)
            end
          ensure
            @hedge_connection = hedge_conn
          end
        end

        # connections should not merge with their hedge connections
        def mergeable?(connection)
          super && connection.hedge_connection != self
        end

        def send(request)
          super.tap do
            if @hedge_connection && !request.hedge_request
              request.hedge_request = hedge_request = request.dup
              request.remember_hedge_connections(self, @hedge_connection)
              @hedge_connection.send(hedge_request)
            end
          end
        end

        private

        def send_request_to_parser(request, *)
          # if original request was in the queue, but has since been finished
          # in the hedge connection. A response which was merely rejected by the
          # +:accept_response+ policy does not count: the remaining attempt must
          # still be allowed to run.
          response = request.response
          return false if response&.finished? && request.hedge_accepted_response?(response)

          super
        end
      end
    end
    register_plugin(:hedging, Hedging)
  end
end
