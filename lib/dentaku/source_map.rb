module Dentaku
  # Maps character offsets in a rewritten string back to the offsets of the
  # original string the user typed. The tokenizer rewrites the input before
  # scanning (stripping comments, expanding aliases); every rewrite records
  # a SourceMap so tokens can still be located in the original formula.
  #
  # Offsets are character offsets (String#length semantics), so multi-byte
  # characters such as Chinese identifiers do not shift positions.
  class SourceMap
    # [target_start, source_start, target_length, source_length]
    Segment = Struct.new(:target_start, :source_start, :target_length, :source_length)

    def self.rewrite(input, pattern)
      segments = []
      output = +""
      cursor = 0

      input.gsub(pattern) do |match|
        match_start = Regexp.last_match.begin(0)
        replacement = yield(match).to_s

        if match_start > cursor
          length = match_start - cursor
          segments << Segment.new(output.length, cursor, length, length)
          output << input[cursor...match_start]
        end

        segments << Segment.new(output.length, match_start, replacement.length, match.length)
        output << replacement
        cursor = match_start + match.length
      end

      if cursor < input.length
        length = input.length - cursor
        segments << Segment.new(output.length, cursor, length, length)
        output << input[cursor..]
      end

      [output, new(segments)]
    end

    def initialize(segments = [])
      @segments = segments
    end

    # map the start offset of a range in the rewritten string
    def map_begin(offset)
      segment = @segments.find { |s| offset >= s.target_start && offset < s.target_start + s.target_length } ||
                @segments.find { |s| offset <= s.target_start } ||
                @segments.last
      interpolate(segment, offset)
    end

    # map the (exclusive) end offset of a range in the rewritten string
    def map_end(offset)
      segment = @segments.find { |s| offset > s.target_start && offset <= s.target_start + s.target_length } ||
                @segments.find { |s| offset <= s.target_start } ||
                @segments.last
      interpolate(segment, offset)
    end

    private

    def interpolate(segment, offset)
      return offset unless segment
      return segment.source_start if segment.target_length.zero?

      segment.source_start + (offset - segment.target_start) * segment.source_length / segment.target_length
    end
  end
end
