module Dentaku
  # Maps character offsets in the alias-expanded input back to offsets in
  # the user's original expression. Alias replacement changes the length
  # of the scanned text, so every offset recorded while scanning has to be
  # translated through the replacements that produced it.
  class OffsetMap
    def self.identity
      new([])
    end

    # replacements: [[original_start, original_length, replacement_length], ...]
    # in the order they appear in the source
    def initialize(replacements)
      @segments = []
      expanded_pos  = 0
      original_pos  = 0

      replacements.each do |original_start, original_length, replacement_length|
        expanded_pos += original_start - original_pos
        @segments << [original_start, original_length, expanded_pos, replacement_length]
        original_pos  = original_start + original_length
        expanded_pos += replacement_length
      end
    end

    def translate(expanded_offset)
      delta = 0

      @segments.each do |original_start, original_length, expanded_start, replacement_length|
        break if expanded_offset < expanded_start

        if expanded_offset <= expanded_start + replacement_length
          # inside a replacement: interpolate so a token covering the whole
          # expansion maps back to the whole original alias
          ratio = (expanded_offset - expanded_start).to_f / [replacement_length, 1].max
          return original_start + (ratio * original_length).round
        end

        delta = (original_start + original_length) - (expanded_start + replacement_length)
      end

      expanded_offset + delta
    end
  end
end
