# frozen_string_literal: true

module I18n
  class MessagePattern
    # Raised when a message pattern cannot be parsed. Carries the exact
    # character position of the problem and renders a caret pointing at it.
    class SyntaxError < I18n::ArgumentError
      attr_reader :pattern, :position

      def initialize(pattern, position, detail)
        @pattern = pattern
        @position = position
        super(build_message(pattern, position, detail))
      end

      private

      def build_message(pattern, position, detail)
        line = pattern[0...position].count("\n") + 1
        line_start = (pattern.rindex("\n", position - 1) || -1) + 1
        line_end = pattern.index("\n", position) || pattern.length
        column = position - line_start + 1
        source_line = pattern[line_start...line_end]

        <<~MESSAGE.chomp
          #{detail} (line #{line}, column #{column})
            #{source_line}
            #{' ' * (column - 1)}^
        MESSAGE
      end
    end
  end
end
