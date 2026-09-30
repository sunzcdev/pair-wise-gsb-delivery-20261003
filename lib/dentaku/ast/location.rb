module Dentaku
  module AST
    # Character span of an AST node inside the original formula string.
    # Offsets are zero-based character positions; line/column are one-based.
    class Location
      attr_reader :offset, :length, :line, :column, :source

      def initialize(offset, length, source = nil)
        @offset = offset
        @length = length
        @source = source

        prefix = source && offset ? source[0, offset] : ""
        @line   = prefix.count("\n") + 1
        @column = offset.to_i - (prefix.rindex("\n") || -1)
      end

      def self.span(*locations, source: nil)
        locations = locations.compact
        return nil if locations.empty?

        start_offset = locations.map(&:offset).min
        end_offset = locations.map { |loc| loc.offset + loc.length }.max
        new(start_offset, end_offset - start_offset, source)
      end

      def end_offset
        offset + length
      end

      def near_source
        return nil unless source

        snippet = source[offset, length].to_s
        snippet.length > 40 ? "#{snippet[0, 37]}..." : snippet
      end

      def to_h
        { offset: offset, length: length, line: line, column: column }
      end
    end
  end
end
