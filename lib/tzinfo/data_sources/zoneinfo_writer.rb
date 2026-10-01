# encoding: UTF-8
# frozen_string_literal: true

module TZInfo
  module DataSources
    # Writes zoneinfo TZif files (versions 2 and 3 of the format described in
    # RFC 9636).
    #
    # Leap second data is not supported. The files produced always contain
    # timestamps that ignore leap seconds (i.e. each day is treated as if it
    # were 86,400 seconds long). Version 4 format features (the footer
    # extension and expiration indicator) are not supported.
    #
    # Data that cannot be represented in the TZif format (for example,
    # transition times outside of the signed 64-bit range, observed UTC
    # offsets outside of the signed 32-bit range, abbreviations containing
    # null characters, or a POSIX-style TZ string that contradicts the last
    # explicit transition) will cause an {ArgumentError} to be raised. Data
    # is never silently truncated or discarded.
    class ZoneinfoWriter
      # The minimum transition time that can be represented in the 32-bit
      # section of a TZif file.
      MIN_INT32 = -2147483648
      private_constant :MIN_INT32

      # The maximum transition time that can be represented in the 32-bit
      # section of a TZif file.
      MAX_INT32 = 2147483647
      private_constant :MAX_INT32

      # Initializes a new {ZoneinfoWriter}.
      #
      # @param posix_tz_parser [PosixTimeZoneParser] a {PosixTimeZoneParser}
      #   instance to use to validate POSIX-style TZ strings.
      def initialize(posix_tz_parser)
        @posix_tz_parser = posix_tz_parser
      end

      # Writes a TZif version 2 or 3 file defining the given offsets and
      # transitions.
      #
      # The file will contain a 32-bit (version 1 compatible) data block
      # holding the transitions with times that can be represented as signed
      # 32-bit integers, followed by a 64-bit data block holding all the
      # transitions, and a footer containing the POSIX-style TZ string (or an
      # empty string if none is given).
      #
      # The offset in effect before the first transition (the
      # {TimezoneTransition#previous_offset previous_offset} of the first
      # transition, or the first non-DST offset when there are no
      # transitions) is placed first in the table of local time types, as
      # expected by TZif readers.
      #
      # @param file_path [String] the path of the file to write.
      # @param offsets [Array<TimezoneOffset>] the offsets (local time types)
      #   to include in the file. Must include every offset referenced by
      #   `transitions`.
      # @param transitions [Array<TimezoneTransition>] the transitions to
      #   include in the file, ordered by increasing time.
      # @param tz_string [String] an optional POSIX-style TZ string for the
      #   footer of the file, describing the offsets and rules that apply
      #   after the last transition. Must be consistent with the offset of
      #   the last transition.
      # @param version [Integer] the TZif version to write (either 2 or 3).
      #   Version 3 requires `tz_string` to not use the tzfile extensions to
      #   the POSIX format (offsets and transition times must be in the range
      #   0 to 24 hours).
      # @raise [ArgumentError] if the offsets, transitions or POSIX-style TZ
      #   string cannot be represented in the TZif format or the requested
      #   version is not supported.
      def write(file_path, offsets, transitions, tz_string = nil, version = 2)
        unless version == 2 || version == 3
          raise ArgumentError, 'Only TZif versions 2 and 3 are supported'
        end

        if offsets.empty?
          raise ArgumentError, 'At least one offset is required'
        end

        if offsets.length > 256
          raise ArgumentError, 'A maximum of 256 offsets can be written'
        end

        offsets.each do |offset|
          unless offset.kind_of?(TimezoneOffset)
            raise ArgumentError, 'offsets must all be TimezoneOffset instances'
          end

          observed_utc_offset = offset.observed_utc_offset

          unless observed_utc_offset >= MIN_INT32 && observed_utc_offset <= MAX_INT32
            raise ArgumentError, "The observed UTC offset #{observed_utc_offset} is outside the range that can be represented"
          end

          if offset.abbreviation.include?("\0")
            raise ArgumentError, "The abbreviation #{offset.abbreviation.inspect} contains a null character"
          end
        end

        previous_at = nil
        previous_offset = nil

        transitions.each do |transition|
          unless transition.kind_of?(TimezoneTransition)
            raise ArgumentError, 'transitions must all be TimezoneTransition instances'
          end

          at = transition.timestamp_value

          unless at >= -0x8000000000000000 && at <= 0x7FFFFFFFFFFFFFFF
            raise ArgumentError, "The transition time #{at} is outside the range that can be represented"
          end

          if previous_at && at <= previous_at
            raise ArgumentError, "The transition at #{at} is not later than the previous transition at #{previous_at}"
          end

          unless offsets.any? {|o| o == transition.offset }
            raise ArgumentError, "The offset of the transition at #{at} is not included in offsets"
          end

          if previous_offset && transition.previous_offset != previous_offset
            raise ArgumentError, "The previous offset of the transition at #{at} does not match the offset of the previous transition"
          end

          previous_at = at
          previous_offset = transition.offset
        end

        if transitions.empty?
          initial_offset = offsets.find {|o| !o.dst? } || offsets[0]
        else
          initial_offset = transitions[0].previous_offset

          unless offsets.any? {|o| o == initial_offset }
            raise ArgumentError, 'The previous offset of the first transition is not included in offsets'
          end
        end

        if tz_string && !tz_string.empty?
          if tz_string.include?("\n")
            raise ArgumentError, 'The POSIX-style TZ string must not contain newlines'
          end

          begin
            rules = @posix_tz_parser.parse(tz_string)
          rescue InvalidPosixTimeZone => e
            raise ArgumentError, "Invalid POSIX-style TZ string: #{e}"
          end

          if rules
            validate_rules_against_transitions(rules, initial_offset, transitions)
            validate_rules_for_version_3(rules) if version == 3
          end
        else
          tz_string = nil
        end

        # The offset in effect before the first transition is placed first in
        # the table of local time types (matching the convention used by zic
        # and expected by TZif readers).
        ordered_offsets = [initial_offset]
        offsets.each do |offset|
          ordered_offsets << offset unless ordered_offsets.any? {|o| o == offset }
        end

        abbreviations = []
        ordered_offsets.each do |offset|
          abbreviation = offset.abbreviation
          abbreviations << abbreviation unless abbreviations.include?(abbreviation)
        end

        abbreviation_offsets = {}
        abbreviation_bytes = String.new
        abbreviations.each do |abbreviation|
          abbreviation_offsets[abbreviation] = abbreviation_bytes.bytesize
          abbreviation_bytes << abbreviation << "\0"
        end

        if transitions.length >= 0x100000000 || abbreviation_bytes.bytesize >= 0x100000000
          raise ArgumentError, 'The number of transitions and abbreviation characters must fit into 32-bit unsigned integers'
        end

        offset_indices = {}
        ordered_offsets.each_with_index do |offset, index|
          offset_indices[offset] = index
        end

        type_bytes = ordered_offsets.map do |offset|
          [offset.observed_utc_offset, offset.dst? ? 1 : 0, abbreviation_offsets[offset.abbreviation]].pack('l>CC')
        end.join

        # The 32-bit block contains the transitions with times that can be
        # represented as signed 32-bit integers (a truncation of the 64-bit
        # block, as required by RFC 9636).
        b32_transitions = transitions.select {|t| t.timestamp_value >= MIN_INT32 && t.timestamp_value <= MAX_INT32 }

        output = String.new
        output << header(version, b32_transitions.length, ordered_offsets.length, abbreviation_bytes.bytesize)
        output << b32_transitions.map {|t| t.timestamp_value }.pack('l>*')
        output << b32_transitions.map {|t| offset_indices[t.offset] }.pack('C*')
        output << type_bytes
        output << abbreviation_bytes
        output << header(version, transitions.length, ordered_offsets.length, abbreviation_bytes.bytesize)
        output << transitions.map {|t| t.timestamp_value }.pack('q>*')
        output << transitions.map {|t| offset_indices[t.offset] }.pack('C*')
        output << type_bytes
        output << abbreviation_bytes
        output << "\n" << (tz_string || '') << "\n"

        File.open(file_path, 'wb') {|file| file.write(output) }
        nil
      end

      private

      # Returns a TZif header. ttisutccnt, ttisstdcnt and leapcnt are all 0
      # (no standard/wall or UTC/local indicators and no leap second data).
      #
      # @param version [Integer] the TZif version (2 or 3).
      # @param timecnt [Integer] the number of transitions in the block.
      # @param typecnt [Integer] the number of local time types.
      # @param charcnt [Integer] the number of abbreviation characters.
      # @return [String] the header.
      def header(version, timecnt, typecnt, charcnt)
        ["TZif#{version}", 0, 0, 0, timecnt, typecnt, charcnt].pack('a5x15N6')
      end

      # Determines if an offset matches an offset derived from the
      # POSIX-style TZ string. This is a looser match than
      # {TimezoneOffset#==}, not requiring the base_utc_offset and std_offset
      # to match (the TZ string only defines the observed UTC offset, DST
      # status and abbreviation).
      #
      # @param offset [TimezoneOffset] an offset defined in the file.
      # @param rule_offset [TimezoneOffset] an offset derived from the
      #   POSIX-style TZ string.
      # @return [Boolean] whether the offsets match.
      def offset_matches_rule?(offset, rule_offset)
        offset.observed_utc_offset == rule_offset.observed_utc_offset &&
          offset.dst? == rule_offset.dst? &&
          offset.abbreviation == rule_offset.abbreviation
      end

      # Validates that the offsets and rules derived from the POSIX-style TZ
      # string do not contradict the explicit transitions (the TZ string
      # describes the offsets and rules that apply after the last explicit
      # transition).
      #
      # @param rules [Object] either a {TimezoneOffset} specifying a constant
      #   offset or an {AnnualRules} instance specifying transitions.
      # @param initial_offset [TimezoneOffset] the offset in effect before
      #   the first transition.
      # @param transitions [Array<TimezoneTransition>] the explicit
      #   transitions.
      # @raise [ArgumentError] if the rules contradict the transitions.
      def validate_rules_against_transitions(rules, initial_offset, transitions)
        if transitions.empty?
          if rules.kind_of?(TimezoneOffset)
            unless offset_matches_rule?(initial_offset, rules)
              raise ArgumentError, 'The constant offset POSIX-style TZ string does not match the offsets'
            end
          else
            first_generated = rules.transitions(1970)[0]

            unless offset_matches_rule?(initial_offset, first_generated.previous_offset)
              raise ArgumentError, 'The offset in effect prior to the first transition generated from the POSIX-style TZ string does not match the offsets'
            end
          end
        else
          last_offset = transitions[-1].offset

          if rules.kind_of?(TimezoneOffset)
            unless offset_matches_rule?(last_offset, rules)
              raise ArgumentError, 'The constant offset POSIX-style TZ string contradicts the offset of the last transition'
            end
          else
            year = transitions[-1].local_end_at.to_time.year
            generated = rules.transitions(year) + rules.transitions(year + 1)
            first_after = generated.find {|t| t.timestamp_value > transitions[-1].timestamp_value }

            unless first_after && offset_matches_rule?(last_offset, first_after.previous_offset)
              raise ArgumentError, 'The POSIX-style TZ string contradicts the offset of the last transition'
            end
          end
        end
      end

      # Validates that the offsets and rules derived from the POSIX-style TZ
      # string conform to the POSIX format (version 3 files must not use the
      # tzfile extensions to the POSIX format).
      #
      # @param rules [Object] either a {TimezoneOffset} specifying a constant
      #   offset or an {AnnualRules} instance specifying transitions.
      # @raise [ArgumentError] if the rules use extensions to the POSIX
      #   format.
      def validate_rules_for_version_3(rules)
        rule_offsets = rules.kind_of?(AnnualRules) ? [rules.std_offset, rules.dst_offset] : [rules]

        rule_offsets.each do |rule_offset|
          if rule_offset.observed_utc_offset.abs > 86400
            raise ArgumentError, 'Version 3 files require the POSIX-style TZ string to specify offsets of no more than 24 hours'
          end
        end

        if rules.kind_of?(AnnualRules)
          [rules.dst_start_rule, rules.dst_end_rule].each do |rule|
            unless rule.transition_at >= 0 && rule.transition_at <= 86400
              raise ArgumentError, 'Version 3 files require the POSIX-style TZ string to specify transition times of 0 to 24 hours'
            end
          end
        end
      end
    end
  end
end
