module VCR
  # @private
  class RequestHandler
    include Logger::Mixin

    def handle
      log "Handling request: #{request_summary} (disabled: #{disabled?})"
      invoke_before_request_hook

      req_type = request_type(:consume_stub)

      log "Identified request type (#{req_type}) for #{request_summary}"

      # The before_request hook can change the type of request
      # (i.e. by inserting a cassette), so we need to query the
      # request type again.
      #
      # Likewise, the main handler logic can modify what
      # #request_type would return (i.e. when a response stub is
      # used), so we need to store the request type for the
      # after_request hook.
      set_typed_request_for_after_hook(req_type)

      response = deliver_response(req_type)

      # A matching interaction was only reserved while building the response.
      # Handlers that deliver the response after #handle returns (such as the
      # WebMock hook) take over the reservation and confirm delivery
      # themselves; for everyone else a successful return is the delivery.
      commit_playback unless defer_playback_commit?

      response
    end

    # @return [HTTPInteractionList::Playback, nil] the interaction reserved
    #  for this request, if any.
    # @private
    attr_reader :playback

    # Confirms that the response was delivered, consuming the reserved
    # interaction. Idempotent.
    #
    # @private
    def commit_playback
      playback.commit! if playback
    end

    # Returns the reserved interaction so that a later request can replay it.
    # Idempotent, and a no-op once the playback was committed.
    #
    # @private
    def release_playback
      playback.release! if playback
    end

  private

    def deliver_response(req_type)
      delivered = false

      begin
        response = send "on_#{req_type}_request"
        delivered = true
        response
      ensure
        # The response never made it to the caller, so the interaction must
        # stay available for a later request (e.g. a retry).
        release_playback unless delivered
      end
    end

    # Handlers can override this when the response they return to the library
    # is not yet the response the caller receives, and delivery can still
    # fail. Such handlers must call {#commit_playback} and {#release_playback}
    # at their own delivery boundary.
    def defer_playback_commit?
      false
    end

    def set_typed_request_for_after_hook(request_type)
      @after_hook_typed_request = Request::Typed.new(vcr_request, request_type)
    end

    def request_type(consume_stub = false)
      case
        when externally_stubbed?                then :externally_stubbed
        when should_ignore?                     then :ignored
        when has_response_stub?(consume_stub)   then :stubbed_by_vcr
        when VCR.real_http_connections_allowed? then :recordable
        else                                         :unhandled
      end
    end

    def invoke_before_request_hook
      return if disabled? || !VCR.configuration.has_hooks_for?(:before_http_request)
      typed_request = Request::Typed.new(vcr_request, request_type)
      VCR.configuration.invoke_hook(:before_http_request, typed_request)
    end

    def invoke_after_request_hook(vcr_response)
      return if disabled?
      VCR.configuration.invoke_hook(:after_http_request, @after_hook_typed_request, vcr_response)
    end

    def externally_stubbed?
      false
    end

    def should_ignore?
      disabled? || VCR.request_ignorer.ignore?(vcr_request)
    end

    def disabled?
      VCR.library_hooks.disabled?(library_name)
    end

    def has_response_stub?(consume_stub)
      if consume_stub
        !checkout_playback.nil?
      else
        VCR.http_interactions.has_interaction_matching?(vcr_request)
      end
    end

    # Reserves the matching interaction for this request. The interaction is
    # only consumed once the response has been delivered.
    def checkout_playback
      return @playback if defined?(@playback)
      @playback = VCR.http_interactions.checkout(vcr_request)
    end

    def stubbed_response
      playback = checkout_playback
      playback && playback.response
    end

    def library_name
      # extracts `:typhoeus` from `VCR::LibraryHooks::Typhoeus::RequestHandler`
      @library_name ||= self.class.name.split('::')[-2].downcase.to_sym
    end

    # Subclasses can implement these
    def on_externally_stubbed_request
    end

    def on_ignored_request
    end

    def on_stubbed_by_vcr_request
    end

    def on_recordable_request
    end

    def on_unhandled_request
      raise VCR::Errors::UnhandledHTTPRequestError.new(vcr_request)
    end

    def request_summary
      request_matchers = if cass = VCR.current_cassette
        cass.match_requests_on
      else
        VCR.configuration.default_cassette_options[:match_requests_on]
      end

      super(vcr_request, request_matchers)
    end

    def log_prefix
      "[#{library_name}] "
    end
  end
end
