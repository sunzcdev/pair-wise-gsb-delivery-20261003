require 'vcr/util/version_checker'
require 'vcr/request_handler'
require 'webmock'

VCR::VersionChecker.new('WebMock', WebMock.version, '1.8.0').check_version!

WebMock.enable!

module VCR
  class LibraryHooks
    # @private
    module WebMock
      extend self

      def with_global_hook_disabled(request)
        global_hook_disabled_requests << request

        begin
          yield
        ensure
          global_hook_disabled_requests.delete(request)
        end
      end

      def global_hook_disabled?(request)
        requests = Thread.current[:_vcr_webmock_disabled_requests]
        requests && requests.include?(request)
      end

      def global_hook_disabled_requests
        Thread.current[:_vcr_webmock_disabled_requests] ||= []
      end

      # Interactions reserved while WebMock builds a stubbed response are
      # attached to the request signature WebMock itself carries from stub
      # resolution to its callbacks. Those callbacks run when WebMock is ready
      # to deliver the response, so the reservation is settled there: committed
      # when the callbacks complete, released when one of them raises (so a
      # retry can still replay it).
      #
      # Attaching the reservation to the signature (rather than keeping a
      # registry) scopes its lifetime to the request and keeps identical
      # concurrent requests from sharing ownership of an interaction.
      #
      # @private
      PLAYBACK_IVAR = :@__vcr_playback

      # @private
      def register_pending_playback(request_signature, playback)
        previous = request_signature.instance_variable_get(PLAYBACK_IVAR)
        # A signature can be reused across attempts (redirects, retries). A
        # reservation still pending at that point never reached delivery.
        previous.release! if previous
        request_signature.instance_variable_set(PLAYBACK_IVAR, playback)
      end

      # @private
      def pending_playback_for?(request_signature)
        request_signature.instance_variable_defined?(PLAYBACK_IVAR)
      end

      # @private
      def settle_pending_playback(request_signature, delivered)
        return unless request_signature.instance_variable_defined?(PLAYBACK_IVAR)
        playback = request_signature.send(:remove_instance_variable, PLAYBACK_IVAR)
        delivered ? playback.commit! : playback.release!
      end

      # Settles a pending playback at WebMock's delivery boundary.
      #
      # @private
      module DeliveryTracking
        def invoke_callbacks(options, request_signature, response)
          track = VCR::LibraryHooks::WebMock.pending_playback_for?(request_signature)
          delivered = false
          result = super
          delivered = true
          result
        ensure
          VCR::LibraryHooks::WebMock.settle_pending_playback(request_signature, delivered) if track
        end
      end

      ::WebMock::CallbackRegistry.singleton_class.prepend(DeliveryTracking)

      # @private
      module Helpers
        def vcr_request_for(webmock_request)
          VCR::Request.new \
            webmock_request.method,
            webmock_request.uri.to_s,
            webmock_request.body,
            request_headers_for(webmock_request)
        end

        # @private
        def vcr_response_for(webmock_response)
          VCR::Response.new \
            VCR::ResponseStatus.new(*webmock_response.status),
            webmock_response.headers,
            webmock_response.body,
            nil
        end

        if defined?(::Excon)
          # @private
          def request_headers_for(webmock_request)
            return nil unless webmock_request.headers

            # WebMock hooks deeply into a Excon at a place where it manually adds a "Host"
            # header, but this isn't a header we actually care to store...
            webmock_request.headers.dup.tap do |headers|
              headers.delete("Host")
            end
          end
        else
          # @private
          def request_headers_for(webmock_request)
            webmock_request.headers
          end
        end

        def typed_request_for(webmock_request, remove = false)
          if webmock_request.instance_variables.find { |v| v.to_sym == :@__typed_vcr_request }
            meth = remove ? :remove_instance_variable : :instance_variable_get
            return webmock_request.send(meth, :@__typed_vcr_request)
          end

          warn <<-EOS.gsub(/^\s+\|/, '')
            |WARNING: There appears to be a bug in WebMock's after_request hook
            |         and VCR is attempting to work around it. Some VCR features
            |         may not work properly.
          EOS

          Request::Typed.new(vcr_request_for(webmock_request), :unknown)
        end
      end

      class RequestHandler < ::VCR::RequestHandler
        include Helpers

        attr_reader :request
        def initialize(request)
          @request = request
        end

      private

        # WebMock asks us for a stubbed response and then goes on to build the
        # library-specific response object and hand it to the test. That
        # second step can still fail, so the interaction is only consumed once
        # WebMock reaches its callback/delivery boundary (see DeliveryTracking).
        def defer_playback_commit?
          ::WebMock::CallbackRegistry.any_callbacks?
        end

        def externally_stubbed?
          # prevent infinite recursion...
          VCR::LibraryHooks::WebMock.with_global_hook_disabled(request) do
            ::WebMock.registered_request?(request)
          end
        end

        def set_typed_request_for_after_hook(*args)
          super
          request.instance_variable_set(:@__typed_vcr_request, @after_hook_typed_request)
        end

        def vcr_request
          @vcr_request ||= vcr_request_for(request)
        end

        def on_externally_stubbed_request
          # nil allows WebMock to handle the request
          nil
        end

        def on_unhandled_request
          invoke_after_request_hook(nil)
          super
        end

        def on_stubbed_by_vcr_request
          response = {
            body:    stubbed_response.body.dup, # Excon mutates the body, so we must dup it :-(
            status:  [stubbed_response.status.code.to_i, stubbed_response.status.message],
            headers: stubbed_response.headers
          }

          if playback && defer_playback_commit?
            VCR::LibraryHooks::WebMock.register_pending_playback(request, playback)
          end

          response
        end
      end

      extend Helpers

      ::WebMock.globally_stub_request do |req|
        global_hook_disabled?(req) ? nil : RequestHandler.new(req).handle
      end

      ::WebMock.after_request(real_requests_only: true) do |request, response|
        unless VCR.library_hooks.disabled?(:webmock)
          http_interaction = VCR::HTTPInteraction.new \
            typed_request_for(request), vcr_response_for(response)

          VCR.record_http_interaction(http_interaction)
        end
      end

      ::WebMock.after_request do |request, response|
        unless VCR.library_hooks.disabled?(:webmock)
          VCR.configuration.invoke_hook \
            :after_http_request,
            typed_request_for(request, :remove),
            vcr_response_for(response)
        end
      end
    end
  end
end

# @private
module WebMock
  class << self
    # ensure HTTP requests are always allowed; VCR takes care of disallowing
    # them at the appropriate times in its hook
    def net_connect_allowed_with_vcr?(*args)
      VCR.turned_on? ? true : net_connect_allowed_without_vcr?(*args)
    end

    alias net_connect_allowed_without_vcr? net_connect_allowed?
    alias net_connect_allowed? net_connect_allowed_with_vcr?
  end unless respond_to?(:net_connect_allowed_with_vcr?)
end

