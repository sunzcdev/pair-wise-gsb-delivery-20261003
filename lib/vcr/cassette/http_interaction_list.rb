require 'vcr/cassette/rehearsal'

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
        def snapshot_for_rehearsal(*a); [[], []]; end
        def request_matchers; []; end
        def allow_playback_repeats; false; end
        def label; nil; end
      end

      attr_reader :interactions, :request_matchers, :allow_playback_repeats, :parent_list

      # @return [String, nil] a human readable name for this list (usually the
      #  cassette name). Used by rehearsal reports.
      attr_reader :label

      def initialize(interactions, request_matchers, allow_playback_repeats = false, parent_list = NullList, log_prefix = '', label: nil)
        @interactions           = interactions.dup
        @request_matchers       = request_matchers
        @allow_playback_repeats = allow_playback_repeats
        @parent_list            = parent_list
        @used_interactions      = []
        @log_prefix             = log_prefix
        @label                  = label
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
            interaction.response
          elsif interaction = matching_used_interaction_for(request)
            interaction.response
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

      # @return [Array(Array, Array)] a consistent, read-only view of the
      #  interactions that are still unused and those that have already been
      #  played back. Neither array aliases the list's internal state, so a
      #  rehearsal can consume from it without affecting real playback.
      # @private
      def snapshot_for_rehearsal
        @mutex.synchronize { [@interactions.dup, @used_interactions.dup] }
      end

      # Builds a read-only rehearsal against this list and the parent lists it
      # falls back to. Each call returns a fresh rehearsal; use {#rehearse},
      # {#rehearse_all} or {#dry_run} with requests to step through one shared
      # virtual state.
      #
      # @param match_requests_on [Array<Symbol, #call>, nil] matchers to use for
      #  the rehearsal. Defaults to nil, which uses each list's own matchers.
      # @return [VCR::Cassette::Rehearsal]
      def rehearsal(match_requests_on: nil)
        Rehearsal.new(self, match_requests_on: match_requests_on)
      end

      # Rehearses one request against this list (and its parents) without
      # consuming anything for real. Successive calls share one virtual state,
      # so repeated requests follow `:allow_playback_repeats` exactly like real
      # playback.
      #
      # @param request [VCR::Request]
      # @return [VCR::Cassette::Rehearsal::Result]
      def rehearse(request)
        rehearsal_state.rehearse(request)
      end

      # Rehearses a whole expected sequence, in order.
      #
      # @param requests [Array<VCR::Request>]
      # @return [VCR::Cassette::Rehearsal::Transcript]
      def rehearse_all(requests)
        rehearsal_state.rehearse_all(requests)
      end

      # A read-only dry run. With no argument it returns a fresh
      # {VCR::Cassette::Rehearsal}; with a single request it returns that
      # request's {VCR::Cassette::Rehearsal::Result}; with a list of requests it
      # returns the {VCR::Cassette::Rehearsal::Transcript} for the sequence.
      #
      # @return [VCR::Cassette::Rehearsal, VCR::Cassette::Rehearsal::Result, VCR::Cassette::Rehearsal::Transcript]
      def dry_run(*requests, match_requests_on: nil)
        return Rehearsal.new(self, match_requests_on: match_requests_on) if requests.empty?

        state = match_requests_on ? Rehearsal.new(self, match_requests_on: match_requests_on) : rehearsal_state
        flattened = requests.flatten(1)

        return state.rehearse(flattened.first) if flattened.size == 1 && !requests.first.is_a?(Array)
        state.rehearse_all(flattened)
      end
      alias preflight dry_run

      # @return [Integer] the virtual number of unused interactions left in this
      #  list after the rehearsals performed on it so far.
      def virtual_remaining_unused_interaction_count
        rehearsal_state.remaining_unused_interaction_count
      end

      # Forgets the virtual consumption recorded by {#rehearse} / {#dry_run}.
      # @return [self]
      def reset_rehearsal!
        @rehearsal_state = nil
        self
      end

      # @private
      def rehearsal_state
        @rehearsal_state ||= Rehearsal.new(self)
      end

      # @private
      # @return [Boolean] whether the recorded interaction matches the given
      #  request using the given matchers (defaulting to the list's own).
      def interaction_matches_request?(request, interaction, matchers = @request_matchers)
        log "Checking if #{request_summary(request)} matches #{request_summary(interaction.request)} using #{matchers.inspect}", 1
        matchers.all? do |matcher_name|
          matcher = VCR.request_matchers[matcher_name]
          matcher.matches?(request, interaction.request).tap do |matched|
            matched = matched ? 'matched' : 'did not match'
            log "#{matcher_name} (#{matched}): current request #{request_summary(request)} vs #{request_summary(interaction.request)}", 2
          end
        end
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
        @interactions.index { |i| interaction_matches_request?(request, i) }
      end

      def matching_used_interaction_for(request)
        return nil unless @allow_playback_repeats
        @used_interactions.find { |i| interaction_matches_request?(request, i) }
      end

      def log_prefix
        @log_prefix
      end
    end
  end
end

