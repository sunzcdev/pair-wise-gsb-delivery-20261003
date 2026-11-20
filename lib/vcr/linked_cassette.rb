require 'delegate'
require 'vcr/errors'

module VCR
  # A Cassette wrapper for linking cassettes from another thread.
  #
  # A linked cassette is owned by another thread (the "parent" context), so it
  # is strictly read-only from this context's point of view: its recorded
  # interactions are still available for playback (including requests that
  # fall through the context's own cassettes via the parent interaction list),
  # but new interactions can never be stored on it or written back to its
  # file. This keeps the shared parent fixture immutable even when a request
  # does not match anything in the current context.
  class LinkedCassette < SimpleDelegator
    # An enumerable lazily wrapping a list of cassettes that a context is using
    class CassetteList
      include Enumerable

      # Creates a new list of context-owned cassettes and linked cassettes
      # @param cassettes [Array] context-owned cassettes
      # @param linked_cassettes [Array] context-unowned (linked) cassettes
      def initialize(cassettes, linked_cassettes)
        @cassettes = cassettes
        @linked_cassettes = linked_cassettes
      end

      # Yields linked cassettes first, and then context-owned cassettes
      def each
        @linked_cassettes.each do |cassette|
          yield wrap(cassette)
        end

        @cassettes.each do |cassette|
          yield cassette
        end
      end

      # Provide last implementation, which is not provided by Enumerable
      def last
        cassette = @cassettes.last
        return cassette if cassette

        cassette = @linked_cassettes.last
        wrap(cassette) if cassette
      end

      # Provide size implementation, which is not provided by Enumerable
      def size
        @cassettes.size + @linked_cassettes.size
      end

    protected
      def wrap(cassette)
        if cassette.linked?
          cassette
        else
          LinkedCassette.new(cassette)
        end
      end
    end

    # Create a new CassetteList
    # @param cassettes [Array] context-owned cassettes
    # @param linked_cassettes [Array] context-unowned (linked) cassettes
    def self.list(cassettes, linked_cassettes)
      CassetteList.new(cassettes, linked_cassettes)
    end

    # Prevents cassette ejection by raising EjectLinkedCassetteError
    def eject(*args)
      raise Errors::EjectLinkedCassetteError,
        "cannot eject a cassette inserted by a parent thread"
    end

    # Linked cassettes are read-only: they may replay interactions but must
    # never accept new ones. The underlying cassette is left untouched so the
    # owning thread can keep recording into its own cassette.
    #
    # @return [Boolean] true
    def read_only?
      true
    end

    # @return [Boolean] false: a linked cassette never records, regardless of
    #  the record mode of the underlying cassette.
    def recording?
      false
    end

    # No-op: interactions are never stored on a linked cassette, so requests
    # that fall through to the shared parent context cannot pollute it.
    #
    # @param interaction [VCR::HTTPInteraction] the interaction that would
    #  have been recorded
    # @return [nil]
    def record_http_interaction(interaction)
      VCR.configuration.logger.log(
        "Ignored record request for linked cassette '#{name}': " \
        "#{VCR.configuration.logger.request_summary(interaction.request, match_requests_on)}",
        "[LinkedCassette: '#{name}'] "
      )
      nil
    end
    # @return [Boolean] true
    def linked?
      true
    end
  end
end
