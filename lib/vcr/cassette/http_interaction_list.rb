module VCR
  class Cassette
    # @private
    class HTTPInteractionList
      include Logger::Mixin

      # @private
      module NullList
        extend self
        def response_for(*a); nil; end
        def checkout(*a); nil; end
        def release_reserved_playbacks!; end
        def has_interaction_matching?(*a); false; end
        def has_used_interaction_matching?(*a); false; end
        def remaining_unused_interaction_count(*a); 0; end
        def remaining_reserved_interaction_count(*a); 0; end
      end

      # A reservation of a single interaction for playback.
      #
      # A playback is handed out by {HTTPInteractionList#checkout}. While it is
      # pending, the checked out interaction is owned by the caller and is not
      # offered to any other checkout (so two concurrent requests can never
      # replay the same non-repeatable interaction). It is only removed from
      # the cassette when the response has actually been delivered, which the
      # caller signals with {#commit!}. If delivery fails or is cancelled, the
      # caller signals {#release!} and the interaction becomes available for
      # matching again, in its original position, so that a retry can replay
      # it.
      class Playback
        # Builds an already delivered playback for a response that was not
        # checked out from this list (e.g. one obtained from a legacy parent
        # list that only implements #response_for). Committing and releasing
        # it are no-ops.
        #
        # @private
        def self.delivered(response)
          new(nil, nil, response: response, repeat: true).tap { |playback| playback.commit! }
        end

        # @return [HTTPInteraction, nil] the reserved interaction, if any.
        attr_reader :interaction

        # @return [VCR::Response] the response to deliver.
        attr_reader :response

        # @private
        def initialize(list, interaction, response: nil, repeat: false)
          @list        = list
          @interaction = interaction
          @response    = response || interaction.response
          @repeat      = repeat
          @state       = :pending
          @mutex       = Mutex.new
        end

        # @return [Boolean] whether this playback reuses an interaction that
        #  was already consumed (only possible with `allow_playback_repeats`).
        def repeat?
          @repeat
        end

        # @return [Boolean] whether the playback is still reserved but neither
        #  committed nor released.
        def pending?
          @mutex.synchronize { @state == :pending }
        end

        # @return [Boolean] whether the response has been successfully delivered
        #  and the interaction consumed.
        def committed?
          @mutex.synchronize { @state == :committed }
        end

        # @return [Boolean] whether the reservation was returned without being
        #  consumed.
        def released?
          @mutex.synchronize { @state == :released }
        end

        # Confirms that the response was delivered, consuming the interaction
        # so that it will not be played back again (unless playback repeats
        # are allowed). This is idempotent.
        #
        # @return [Boolean] whether this call performed the commit.
        def commit!
          return false unless transition_to(:committed)
          @list.commit_playback(self) unless @repeat
          true
        end

        # Returns the reservation, making the interaction available for
        # matching again. This is idempotent and is a no-op once the playback
        # has been committed.
        #
        # @return [Boolean] whether this call performed the release.
        def release!
          return false unless transition_to(:released)
          @list.release_playback(self) unless @repeat
          true
        end

      private

        def transition_to(state)
          @mutex.synchronize do
            return false unless @state == :pending
            @state = state
            true
          end
        end
      end

      attr_reader :interactions, :request_matchers, :allow_playback_repeats, :parent_list

      def initialize(interactions, request_matchers, allow_playback_repeats = false, parent_list = NullList, log_prefix = '')
        @interactions           = interactions.dup
        @request_matchers       = request_matchers
        @allow_playback_repeats = allow_playback_repeats
        @parent_list            = parent_list
        @used_interactions      = []
        @reserved_playbacks     = {}.compare_by_identity
        @log_prefix             = log_prefix
        @mutex                  = Mutex.new

        interaction_summaries = interactions.map { |i| "#{request_summary(i.request)} => #{response_summary(i.response)}" }
        log "Initialized HTTPInteractionList with request matchers #{request_matchers.inspect} and #{interactions.size} interaction(s): { #{interaction_summaries.join(', ')} }", 1
      end

      # Reserves the first matching interaction for playback without consuming
      # it. The returned {Playback} owns the interaction until it is committed
      # or released; concurrent checkouts will not see it in the meantime.
      #
      # @return [Playback, nil] the reservation, or nil if no interaction matches.
      def checkout(request)
        # Without this mutex, under threaded access, the wrong response may be removed
        # out of the (remaining) interactions list (and other problems).
        @mutex.synchronize do
          if index = matching_interaction_index_for(request)
            interaction = @interactions[index]
            playback = Playback.new(self, interaction)
            @reserved_playbacks[playback] = interaction
            log "Found matching interaction for #{request_summary(request)} at index #{index}: #{response_summary(interaction.response)}", 1
            playback
          elsif interaction = matching_used_interaction_for(request)
            Playback.new(self, interaction, repeat: true)
          elsif interaction = matching_reserved_interaction_for(request)
            Playback.new(self, interaction, repeat: true)
          else
            checkout_from_parent(request)
          end
        end
      end

      # Checks out a matching interaction, immediately commits it and returns
      # its response. This is the historical one-step playback API; prefer
      # {#checkout} when the response still has to be delivered and delivery
      # can fail.
      def response_for(request)
        playback = checkout(request)
        return nil unless playback
        playback.commit!
        playback.response
      end

      # @private
      # Consumes a reserved interaction. Called by {Playback#commit!}.
      def commit_playback(playback)
        interaction = playback.interaction
        @mutex.synchronize do
          return false unless @reserved_playbacks.key?(playback)
          @reserved_playbacks.delete(playback)

          index = @interactions.index { |i| i.equal?(interaction) }
          return false unless index

          @interactions.delete_at(index)
          @used_interactions.unshift(interaction)
          true
        end
      end

      # @private
      # Returns a reserved interaction to the cassette, keeping its original
      # position so that ordering of identical-looking interactions does not
      # change. Called by {Playback#release!}.
      def release_playback(playback)
        @mutex.synchronize do
          return false unless @reserved_playbacks.key?(playback)
          @reserved_playbacks.delete(playback)
          true
        end
      end

      # @private
      # Releases every reservation that was never settled. A reservation that is
      # still pending when the cassette is ejected was never delivered, so the
      # interaction must count as unused (and remain replayable if the cassette
      # is loaded again).
      def release_reserved_playbacks!
        playbacks = @mutex.synchronize { @reserved_playbacks.keys }
        playbacks.each(&:release!)
      end

      def has_interaction_matching?(request)
        !!matching_interaction_index_for(request) ||
        !!matching_used_interaction_for(request) ||
        !!matching_reserved_interaction_for(request) ||
        @parent_list.has_interaction_matching?(request)
      end

      def has_used_interaction_matching?(request)
        @used_interactions.any? { |i| interaction_matches_request?(request, i) }
      end

      def remaining_unused_interaction_count
        @interactions.size
      end

      # @return [Integer] how many of the remaining interactions are currently
      #  reserved by another request and therefore not available to this one.
      def remaining_reserved_interaction_count
        @mutex.synchronize { @reserved_playbacks.size }
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

      # @return [Boolean] Whether or not there are unused interactions left in the list.
      def has_unused_interactions?
        @interactions.size > 0
      end

      def request_summary(request)
        super(request, @request_matchers)
      end

      def matching_interaction_index_for(request)
        @interactions.index { |i| !reserved?(i) && interaction_matches_request?(request, i) }
      end

      def matching_used_interaction_for(request)
        return nil unless @allow_playback_repeats
        @used_interactions.find { |i| interaction_matches_request?(request, i) }
      end

      # A reservation that is still pending belongs to the request that checked
      # it out. It can only be handed out again when playback repeats are
      # allowed; otherwise a concurrent request must not replay it.
      def matching_reserved_interaction_for(request)
        return nil unless @allow_playback_repeats
        @reserved_playbacks.values.find do |i|
          interaction_matches_request?(request, i)
        end
      end

      def checkout_from_parent(request)
        if @parent_list.respond_to?(:checkout)
          @parent_list.checkout(request)
        elsif (response = @parent_list.response_for(request))
          Playback.delivered(response)
        end
      end

      def reserved?(interaction)
        @reserved_playbacks.each_value.any? { |reserved| reserved.equal?(interaction) }
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

