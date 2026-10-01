# encoding: UTF-8
# frozen_string_literal: true

module TZInfo
  # A {CorrectionBuilder} is used to accumulate {TimezoneCorrection} entries
  # when constructing an overriding view of a {Timezone} with a block passed
  # to {Timezone#with_corrections}.
  #
  # Within the block, {#add} (or the {#correction} alias) is used to define
  # each errata entry by giving its UTC start time (inclusive), UTC end time
  # (exclusive) and complete offset information. For example:
  #
  #     tz = TZInfo::Timezone.get('America/New_York')
  #     corrected = tz.with_corrections do |c|
  #       c.add('1947-01-01 05:00:00 UTC', '1947-09-28 06:00:00 UTC',
  #             -5*3600, 3600, 'EWT')
  #     end
  class CorrectionBuilder
    # Initializes a new {CorrectionBuilder}.
    #
    # {CorrectionBuilder} instances should not normally be constructed
    # directly. They are yielded by {Timezone#with_corrections} when called
    # with a block.
    def initialize
      @corrections = []
    end

    # Adds a new {TimezoneCorrection} to the set of entries being built.
    #
    # The interval of time over which the correction applies is half-open:
    # the start time is included and the end time is excluded.
    #
    # The complete offset information can either be supplied as an existing
    # {TimezoneOffset} instance or by giving the base (standard time) offset
    # from UTC, the seasonal offset from standard time and an abbreviation.
    #
    # @param (see TimezoneCorrection#initialize)
    # @return [TimezoneCorrection] the {TimezoneCorrection} that was added.
    # @raise (see TimezoneCorrection#initialize)
    # @raise [ArgumentError] if the new correction overlaps with one that has
    #   already been added (touching end-to-start boundaries are allowed).
    def add(start_at, end_at, offset = nil, base_utc_offset = nil, std_offset = nil, abbreviation = nil)
      correction = TimezoneCorrection.new(start_at, end_at, offset, base_utc_offset, std_offset, abbreviation)

      @corrections.each do |existing|
        if correction.start_at.value < existing.end_at.value && existing.start_at.value < correction.end_at.value
          raise ArgumentError, "corrections must not overlap: #{correction.inspect} overlaps #{existing.inspect}"
        end
      end

      @corrections << correction
    end
    alias correction add

    # @return [Array<TimezoneCorrection>] the {TimezoneCorrection} entries that
    #   have been added, sorted by ascending start time.
    def corrections
      @corrections.sort_by {|c| c.start_at.value }.freeze
    end
  end
end
