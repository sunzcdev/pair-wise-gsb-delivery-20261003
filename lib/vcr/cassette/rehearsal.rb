require 'vcr/errors'
require 'vcr/util/logger'

module VCR
  class Cassette
    # A read-only rehearsal (a.k.a. dry run) of playback against a cassette's
    # HTTP interaction list and the parent (outer cassette) lists it falls back
    # to.
    #
    # A rehearsal answers the question "would this request be playable, and from
    # which cassette would the response come?" without touching the cassette:
    # playback counters, cassette hooks and the HTTP connection state are left
    # exactly as they were, and no request is ever sent over the network.
    #
    # Each rehearsed request consumes the matching interaction only virtually,
    # so sequential rehearsals follow the same rules as real playback, including
    # `:allow_playback_repeats` and fallback to a parent cassette:
    #
    #   rehearsal = cassette.rehearsal # or VCR.rehearsal
    #   rehearsal.rehearse(request)    # => available, from the parent cassette
    #   rehearsal.rehearse(request)    # => exhausted (repeats not allowed)
    #   cassette.http_interactions.response_for(request) # still available
    #
    # The virtual interactions left after a rehearsal are available through
    # {#remaining_unused_interaction_counts} and, for a whole sequence,
    # {Transcript#remaining_unused_interaction_count}.
    #
    # @note A rehearsal never loads additional cassettes, never plays a response
    #  back through an HTTP library hook, never runs `before_playback` or
    #  `after_http_request` hooks, and never records anything. It only inspects
    #  the interaction lists that are already in memory.
    class Rehearsal
      include Logger::Mixin

      # The outcome of rehearsing a single request.
      class Result
        # @return [VCR::Request] the rehearsed request.
        attr_reader :request

        # @return [VCR::HTTPInteraction, nil] the interaction that would have
        #  been played back, or nil when the request is not playable.
        attr_reader :interaction

        # @return [VCR::Response, nil] the response that would have been used.
        attr_reader :response

        # @return [Integer, nil] 0 for the cassette's own interaction list, 1 for
        #  its parent (outer) cassette, and so on. nil when nothing matched.
        attr_reader :depth

        # @return [String, nil] a label for the interaction list the match came
        #  from (usually the cassette name).
        attr_reader :label

        # @return [Integer] the virtual number of unused interactions left in the
        #  whole rehearsal chain after this request was rehearsed.
        attr_reader :remaining_unused_interaction_count

        # @private
        def initialize(request, interaction = nil, response = nil, depth = nil, label = nil,
                       repeated = false, remaining_unused_interaction_count = 0)
          @request                            = request
          @interaction                        = interaction
          @response                           = response
          @depth                              = depth
          @label                              = label
          @repeated                           = repeated
          @remaining_unused_interaction_count = remaining_unused_interaction_count
        end

        # @return [Boolean] whether a response could be played back.
        def available?
          !interaction.nil?
        end
        alias matched? available?

        # @return [Boolean] whether the cassette chain ran out of interactions.
        def exhausted?
          !available?
        end

        # @return [Boolean] whether an already used interaction would have been
        #  played back again due to `:allow_playback_repeats`.
        def repeated?
          !!@repeated
        end

        # @return [Boolean] whether the response would have come from a parent
        #  (outer) cassette rather than the current one.
        def from_parent?
          !depth.nil? && depth > 0
        end

        # @return [Symbol] `:self`, `:parent` or `:none`.
        def source
          return :none unless available?
          from_parent? ? :parent : :self
        end

        # @return [Symbol] `:available` or `:exhausted`.
        def outcome
          available? ? :available : :exhausted
        end
        alias status outcome

        # @return [Integer] alias of {#remaining_unused_interaction_count}.
        def virtual_remaining
          remaining_unused_interaction_count
        end

        # @return [Hash] a hash representation suitable for reporting/tests.
        def to_h
          {
            request:                            request,
            outcome:                            outcome,
            depth:                              depth,
            label:                              label,
            source:                             source,
            repeated:                           repeated?,
            interaction:                        interaction,
            response:                           response,
            remaining_unused_interaction_count: remaining_unused_interaction_count
          }
        end

        # @return [String] a one line human readable description of the outcome.
        def to_s
          description = "#{http_method} #{request && request.uri}"

          unless available?
            return "exhausted: #{description} (no playable interaction left)"
          end

          notes = []
          notes << (from_parent? ? "parent cassette: #{label}" : "cassette: #{label}")
          notes << "repeated playback" if repeated?
          notes << "#{remaining_unused_interaction_count} unused interaction(s) remaining"

          "available: #{description} => [#{response_status}] (#{notes.join(', ')})"
        end

      private

        def http_method
          request && request.method ? request.method.to_s.upcase : 'REQUEST'
        end

        def response_status
          response && response.status ? response.status.code : 'no response'
        end
      end

      # The ordered outcome of rehearsing a whole sequence of requests.
      class Transcript
        include Enumerable

        # @return [Array<Result>] one result per rehearsed request, in order.
        attr_reader :results

        # @return [Integer] the virtual unused interaction count before the first
        #  request was rehearsed.
        attr_reader :initial_remaining_unused_interaction_count

        # @return [Integer] the virtual unused interaction count after the last
        #  request was rehearsed.
        attr_reader :remaining_unused_interaction_count

        # @private
        def initialize(results, initial_remaining_unused_interaction_count = 0)
          @results                                 = results
          @initial_remaining_unused_interaction_count = initial_remaining_unused_interaction_count
          @remaining_unused_interaction_count      = if results.empty?
            initial_remaining_unused_interaction_count
          else
            results.last.remaining_unused_interaction_count
          end
        end

        # Yields each result in the order the requests were rehearsed.
        #
        # @yieldparam result [Result]
        # @return [Enumerable, self]
        def each(&block)
          return to_enum(:each) unless block
          results.each(&block)
          self
        end

        # @return [Integer] the number of rehearsed requests.
        def size
          results.size
        end
        alias length size

        # @return [Array<Result>] the requests that would have been played back.
        def available
          results.select(&:available?)
        end

        # @return [Array<Result>] the requests that ran out of interactions.
        def exhausted
          results.reject(&:available?)
        end

        # @return [Integer]
        def available_count
          available.size
        end

        # @return [Integer]
        def exhausted_count
          exhausted.size
        end

        # @return [Array<Symbol>] the outcome of each rehearsed request, in order.
        def outcomes
          results.map(&:outcome)
        end

        # @return [Array<Symbol>] the source of each rehearsed request, in order.
        def sources
          results.map(&:source)
        end

        # @return [Boolean] whether every request would have been playable.
        def all_available?
          exhausted.empty?
        end

        # @return [Boolean] whether at least one request ran out of interactions.
        def exhausted?
          !all_available?
        end

        # @return [Integer] alias of {#remaining_unused_interaction_count}.
        def virtual_remaining
          remaining_unused_interaction_count
        end

        # @return [Array<Hash>] one hash per rehearsed request.
        def details
          results.map(&:to_h)
        end
        alias to_hashes details

        # @return [String] the per request results plus the virtual remaining count.
        def to_s
          lines = results.each_with_index.map { |result, index| "#{index + 1}. #{result}" }
          lines << "remaining unused interactions: " \
                   "#{initial_remaining_unused_interaction_count} => #{remaining_unused_interaction_count}"
          lines.join("\n")
        end

        # @return [self]
        # @raise [VCR::Errors::RehearsalExhaustedError] when a request ran out of
        #  interactions, naming the first such request and its position.
        def assert_all_available!
          return self if all_available?

          index = results.index { |result| !result.available? }
          raise Errors::RehearsalExhaustedError.new(
            results[index].request,
            "No playable HTTP interaction for request #{index + 1} of #{results.size}.\n#{self}"
          )
        end
      end

      # @param list [HTTPInteractionList] the interaction list to rehearse
      #  against. Parent lists are followed, so a nested cassette is rehearsed
      #  exactly the way a real request would be played back.
      # @param match_requests_on [Array<Symbol, #call>, nil] the matchers to use.
      #  Defaults to nil, which uses each interaction list's own
      #  `match_requests_on` (just like real playback).
      def initialize(list, match_requests_on: nil)
        @levels             = collect_levels(list)
        @match_requests_on  = match_requests_on && Array(match_requests_on)
        @virtual_consumed   = {}
        @results            = []
        @mutex              = Mutex.new

        validate_matchers!

        return unless @levels.any?
        log "Initialized rehearsal with request matchers #{request_matchers.inspect} " \
            "over #{@levels.size} interaction list(s)", 1
      end

      # @return [Array<HTTPInteractionList>] the rehearsed list chain: the
      #  cassette's own list first, then its parent (outer) cassettes.
      def levels
        @levels.dup
      end

      # @return [Integer] the number of cassettes taking part in the rehearsal.
      def depth_count
        @levels.size
      end
      alias list_count depth_count

      # @return [Array<Symbol, #call>] the matchers used by the rehearsal.
      def request_matchers
        @match_requests_on || @levels.flat_map(&:request_matchers).uniq
      end

      # Rehearses a single request, consuming the matching interaction only
      # virtually.
      #
      # @param request [VCR::Request] the request to rehearse.
      # @return [Result]
      def rehearse(request)
        result = @mutex.synchronize { rehearse_without_mutex(request) }
        log "Rehearsed #{format_request(request)}: #{result}", 1
        result
      end
      alias preflight rehearse
      alias dry_run rehearse

      # Like {#rehearse}, but raises when the request is not playable.
      #
      # @param request [VCR::Request]
      # @return [Result]
      # @raise [VCR::Errors::RehearsalExhaustedError]
      def rehearse!(request)
        result = rehearse(request)
        unless result.available?
          raise Errors::RehearsalExhaustedError.new(
            request,
            "No playable HTTP interaction for #{format_request(request)}.\n#{summary}"
          )
        end
        result
      end

      # Rehearses an expected sequence of requests, one by one, in order.
      #
      # @param requests [Array<VCR::Request>]
      # @return [Transcript]
      def rehearse_all(requests)
        starting_count = total_remaining_unused_interaction_count
        results = Array(requests).map { |request| rehearse(request) }
        Transcript.new(results, starting_count)
      end
      alias preflight_all rehearse_all
      alias rehearse_sequence rehearse_all

      # @return [Array<Result>] the results of every request rehearsed so far
      #  with this rehearsal.
      def results
        @mutex.synchronize { @results.dup }
      end
      alias history results

      # Forgets every virtual consumption, so the rehearsal starts again from
      # the real cassette state. The real cassette is never affected.
      #
      # @return [self]
      def reset!
        @mutex.synchronize do
          @virtual_consumed.clear
          @results.clear
        end
        self
      end

      # @return [Array<Integer>] the virtual unused interaction count of each
      #  cassette in the chain, starting with the current one.
      def remaining_unused_interaction_counts
        @mutex.synchronize { remaining_unused_interaction_counts_without_mutex }
      end

      # @return [Integer] the virtual unused interaction count of the current
      #  cassette's own list.
      def remaining_unused_interaction_count
        remaining_unused_interaction_counts.first || 0
      end

      # @return [Integer] the virtual unused interaction count across the whole
      #  cassette chain.
      def total_remaining_unused_interaction_count
        remaining_unused_interaction_counts.sum
      end
      alias virtual_remaining total_remaining_unused_interaction_count

      # @return [String] a description of the rehearsal and its virtual state.
      def summary
        @mutex.synchronize do
          counts = remaining_unused_interaction_counts_without_mutex
          descriptions = @levels.each_with_index.map do |level, index|
            "#{level.label}: #{counts[index]} unused"
          end

          ["rehearsal over #{@levels.size} interaction list(s)",
           descriptions.empty? ? "no cassette is in use" : descriptions.join(', '),
           "total virtual remaining: #{counts.sum}"].join("\n")
        end
      end

      # @return [String]
      def to_s
        summary
      end

    private

      def collect_levels(list)
        levels = []
        current = list

        while HTTPInteractionList === current
          break if levels.any? { |level| level.equal?(current) }
          levels << current
          current = current.parent_list
        end

        levels
      end

      def validate_matchers!
        return unless @match_requests_on
        @match_requests_on.each { |matcher| VCR.request_matchers[matcher] }
      end

      def matchers_for(index)
        @match_requests_on || @levels[index].request_matchers
      end

      # Builds the interaction lists as they would be if only the rehearsal had
      # consumed interactions. Real playback (including playback that happens
      # while the rehearsal is in use) is reflected but never modified.
      def virtual_state(index)
        unused, used = @levels[index].snapshot_for_rehearsal
        consumed = @virtual_consumed[index] || []

        return [unused, used] if consumed.empty?

        virtual_unused = unused.reject { |interaction| consumed.any? { |c| c.equal?(interaction) } }

        # An interaction that real playback consumed after the rehearsal did is
        # already in `used`; adding it again would make repeats look available
        # twice. Interactions used only virtually go first, because
        # `@used_interactions` keeps the most recently used one first.
        already_used = ->(interaction) { used.any? { |u| u.equal?(interaction) } }
        [virtual_unused, consumed.reject(&already_used) + used]
      end

      def rehearse_without_mutex(request)
        result = nil

        @levels.each_with_index do |level, index|
          matchers = matchers_for(index)
          unused, used = virtual_state(index)

          if (position = index_of_match(level, unused, request, matchers))
            interaction = unused[position]
            (@virtual_consumed[index] ||= []).unshift(interaction)
            result = build_result(request, interaction, index, false)
            break
          elsif level.allow_playback_repeats && (position = index_of_match(level, used, request, matchers))
            result = build_result(request, used[position], index, true)
            break
          end
        end

        result ||= build_result(request, nil, nil, false)
        @results << result
        result
      end

      def build_result(request, interaction, depth, repeated)
        Result.new(
          request,
          interaction,
          interaction && interaction.response,
          depth,
          depth.nil? ? nil : @levels[depth].label,
          repeated,
          remaining_unused_interaction_counts_without_mutex.sum
        )
      end

      def index_of_match(level, interactions, request, matchers)
        interactions.index do |interaction|
          level.interaction_matches_request?(request, interaction, matchers)
        end
      end

      def remaining_unused_interaction_counts_without_mutex
        @levels.each_index.map { |index| virtual_state(index).first.size }
      end

      def format_request(request)
        "#{request.method.to_s.upcase} #{request.uri}"
      end

      def log_prefix
        "[Rehearsal] "
      end
    end
  end
end