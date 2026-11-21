# frozen_string_literal: true

module HTTPX
  module Plugins
    #
    # This plugin adds support for HTTP/2 Push responses.
    #
    # In order to benefit from this, requests are sent one at a time, so that
    # no push responses are received after corresponding request has been sent.
    #
    # https://gitlab.com/os85/httpx/wikis/Server-Push
    #
    module PushPromise
      # Headers which influence the representation a server selects for a
      # response. A pushed stream can only be handed over to an explicit
      # request when these do not conflict, i.e. when the promised request
      # either does not carry them, or carries the exact same value as the
      # request. Otherwise, the pushed response may not be the representation
      # the request asked for, and cannot be delivered to it.
      REPRESENTATION_HEADERS = %w[
        accept
        accept-charset
        accept-encoding
        accept-language
        authorization
        cookie
        if-match
        if-modified-since
        if-none-match
        if-range
        if-unmodified-since
        proxy-authorization
        range
      ].freeze

      def self.extra_options(options)
        options.merge(
          http2_settings: { settings_enable_push: 1 },
          max_concurrent_requests: 1
        )
      end

      module ResponseMethods
        def pushed?
          @__pushed
        end

        def mark_as_pushed!
          @__pushed = true
        end
      end

      module InstanceMethods
        private

        def promise_headers
          @promise_headers ||= {}
        end

        def on_promise(parser, stream)
          stream.on(:promise_headers) do |h|
            __on_promise_request(parser, stream, h)
          end
          stream.on(:headers) do |h|
            __on_promise_response(parser, stream, h)
          end
        end

        def __on_promise_request(parser, stream, h)
          log(level: 1, color: :yellow) do
            # simplecov:disable
            h.map { |k, v| "#{stream.id}: -> PROMISE HEADER: #{k}: #{v}" }.join("\n")
            # simplecov:enable
          end
          headers = @options.headers_class.new(h)
          path = headers[":path"]
          authority = headers[":authority"]

          # a pushed stream can only be delivered to the request waiting for
          # the exact same resource, i.e. matching the explicit request
          # identity: same method, same authority, same path (query params
          # included), and no conflicting representation headers.
          request = find_promised_request(parser, authority, path, headers)

          if request
            promise_headers[stream] = request

            if parser.pending.delete(request)
              # the request was still waiting to be sent; it does not need to
              # go on the wire anymore, the pushed stream will deliver its
              # response instead.
              request.merge_headers(headers)
              parser.streams[request] = stream
              request.transition(:done)
            else
              # the request was already sent on its own stream; hand it over
              # to the pushed stream, and release the stream it was sent on.
              __release_promised_request_stream(parser, request)
              parser.streams[request] = stream
            end
          else
            # no waiter for this push, release the stream right away.
            stream.refuse
          end
        end

        def __on_promise_response(parser, stream, h)
          request = promise_headers.delete(stream)
          return unless request

          parser.__send__(:on_stream_headers, stream, request, h)
          response = request.response
          response.mark_as_pushed!
          stream.on(:data, &parser.method(:on_stream_data).curry(3)[stream, request])
          stream.on(:close, &parser.method(:on_stream_close).curry(3)[stream, request])
        end

        # finds the request waiting for the resource identified by the
        # promised +authority+, +path+ and +headers+, first among the requests
        # still waiting to be sent, then among the requests already sent on
        # the wire.
        def find_promised_request(parser, authority, path, headers)
          parser.pending.find { |r| promised_request_match?(r, authority, path, headers) } ||
            parser.streams.keys.find { |r| promised_request_match?(r, authority, path, headers) }
        end

        def promised_request_match?(request, authority, path, headers)
          return false unless request.authority == authority && request.path == path

          method = headers[":method"]
          return false if method && request.verb.to_s.casecmp(method).nonzero?

          REPRESENTATION_HEADERS.none? do |header|
            headers.key?(header) && request.headers[header] != headers[header]
          end
        end

        # detaches +request+ from the stream it was already sent on, and
        # releases that stream, as the request is being handed over to a
        # pushed stream.
        def __release_promised_request_stream(parser, request)
          stream = parser.streams[request]
          return unless stream

          # detach the request from its in-flight stream before cancelling
          # it, so that the stream termination is not accounted to the
          # request (which is being handed over to a pushed stream).
          parser.__send__(:teardown, request)
          stream.close unless stream.closed?
        end
      end
    end
    register_plugin(:push_promise, PushPromise)
  end
end
