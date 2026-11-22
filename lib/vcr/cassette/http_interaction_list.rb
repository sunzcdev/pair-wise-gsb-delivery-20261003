module VCR
  class Cassette
    # @private
    class HTTPInteractionList
      include Logger::Mixin

      # @private
      module NullList
        extend self
        def response_for(*a); nil; end
        def has_interaction_matching?(*a); false; end
        def has_used_interaction_matching?(*a); false; end
        def remaining_unused_interaction_count(*a); 0; end
      end

      attr_reader :interactions, :request_matchers, :allow_playback_repeats, :parent_list

      def initialize(interactions, request_matchers, allow_playback_repeats = false, parent_list = NullList, log_prefix = '')
        @interactions           = interactions.dup
        @request_matchers       = request_matchers
        @allow_playback_repeats = allow_playback_repeats
        @parent_list            = parent_list
        @used_interactions      = []
        @log_prefix             = log_prefix
        @mutex                  = Mutex.new

        interaction_summaries = interactions.map { |i| "#{request_summary(i.request)} => #{response_summary(i.response)}" }
        log "Initialized HTTPInteractionList with request matchers #{request_matchers.inspect} and #{interactions.size} interaction(s): { #{interaction_summaries.join(', ')} }", 1
      end

      def response_for(request)
        # Without this mutex, under threaded access, the wrong response may be removed
        # out of the (remaining) interactions list (and other problems).
        @mutex.synchronize do
          if index = matching_interaction_index_for(request)
            interaction = @interactions.delete_at(index)
            @used_interactions.unshift interaction
            log "Found matching interaction for #{request_summary(request)} at index #{index}: #{response_summary(interaction.response)}", 1
            deep_duplicate_response(interaction.response)
          elsif interaction = matching_used_interaction_for(request)
            deep_duplicate_response(interaction.response)
          else
            @parent_list.response_for(request)
          end
        end
      end

      def has_interaction_matching?(request)
        !!matching_interaction_index_for(request) ||
        !!matching_used_interaction_for(request) ||
        @parent_list.has_interaction_matching?(request)
      end

      def has_used_interaction_matching?(request)
        @used_interactions.any? { |i| interaction_matches_request?(request, i) }
      end

      def remaining_unused_interaction_count
        @interactions.size
      end

      # Checks if there are no unused interactions left.
      #
      # @raise [VCR::Errors::UnusedHTTPInteractionError] if not all interactions were played back.
      def assert_no_unused_interactions!
        return unless has_unused_interactions?
        logger = Logger.new(nil)

        descriptions = @interactions.map do |i|
          "  - #{logger.request_summary(i.request, @request_matchers)} => #{logger.response_summary(i.response)}"
        end.join("\n")

        raise Errors::UnusedHTTPInteractionError, "There are unused HTTP interactions left in the cassette:\n#{descriptions}"
      end

    private

      # Returns a deep copy of the given response so that mutations made by a
      # `before_playback` hook (or by the code consuming the played-back response,
      # such as changing a header or a JSON string) never leak back into the
      # interaction stored on the cassette. Without this, a repeated playback
      # (when `allow_playback_repeats` is enabled) would hand out the same,
      # already-mutated response object instead of the original fixture data.
      #
      # The copy is built directly from the struct members rather than via
      # `Marshal` (the normalized-header hash carries a default proc, which
      # cannot be marshalled) or `to_hash`/`from_hash` (which consults global
      # VCR configuration for body byte preservation).
      def deep_duplicate_response(response)
        return nil if response.nil?

        VCR::Response.new(
          deep_duplicate(response.status),
          deep_duplicate(response.headers),
          response.body.nil? ? nil : response.body.dup,
          response.http_version,
          deep_duplicate(response.adapter_metadata)
        )
      end

      def deep_duplicate(object)
        case object
        when ::Hash
          object.each_with_object({}) { |(key, value), copy| copy[key] = deep_duplicate(value) }
        when ::Array
          object.map { |value| deep_duplicate(value) }
        when VCR::ResponseStatus
          VCR::ResponseStatus.new(object.code, object.message)
        when ::String
          object.dup
        else
          object
        end
      end

      # @return [Boolean] Whether or not there are unused interactions left in the list.
      def has_unused_interactions?
        @interactions.size > 0
      end

      def request_summary(request)
        super(request, @request_matchers)
      end

      def matching_interaction_index_for(request)
        @interactions.index { |i| interaction_matches_request?(request, i) }
      end

      def matching_used_interaction_for(request)
        return nil unless @allow_playback_repeats
        @used_interactions.find { |i| interaction_matches_request?(request, i) }
      end

      def interaction_matches_request?(request, interaction)
        log "Checking if #{request_summary(request)} matches #{request_summary(interaction.request)} using #{@request_matchers.inspect}", 1
        @request_matchers.all? do |matcher_name|
          matcher = VCR.request_matchers[matcher_name]
          matcher.matches?(request, interaction.request).tap do |matched|
            matched = matched ? 'matched' : 'did not match'
            log "#{matcher_name} (#{matched}): current request #{request_summary(request)} vs #{request_summary(interaction.request)}", 2
          end
        end
      end

      def log_prefix
        @log_prefix
      end
    end
  end
end

