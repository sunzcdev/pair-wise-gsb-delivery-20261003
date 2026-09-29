# frozen_string_literal: true

module I18n
  class MessagePattern
    # Recursive descent parser turning a message pattern string into an AST.
    #
    # Grammar (whitespace is skipped around structural tokens, i.e. anywhere
    # except inside literal text):
    #
    #   message  := part*
    #   part     := text | '{' argument '}'
    #   argument := name (',' kind ',' branches)?
    #   kind     := 'select' | 'plural'
    #   branches := (selector '{' message '}')+
    #   selector := name            (select and plural)
    #             | '=' number      (plural only, exact match)
    #
    # The AST is a nested structure of frozen arrays:
    #
    #   [:text, "literal"]
    #   [:number]                        # '#' inside a plural branch
    #   [:arg, "name"]
    #   [:select, "name", { key => [part] }]
    #   [:plural, "name", { key => [part] }]   # key is a Symbol category or an Integer for '=N'
    class Parser
      PLURAL_CATEGORIES = %i[zero one two few many other].freeze
      NUMBER_NODE = [:number].freeze
      NAME_START = /[A-Za-z_]/.freeze
      NAME_CHAR = /[A-Za-z0-9_]/.freeze
      QUOTABLE = ['{', '}', '#'].freeze

      def self.parse(pattern)
        new(pattern).parse
      end

      def initialize(pattern)
        @pattern = pattern
        @chars = pattern.chars
        @pos = 0
      end

      def parse
        nodes = parse_message(false)
        error("unmatched '}'") if peek == '}'
        nodes
      end

      private

      # Parses literal text and arguments until the end of the input or an
      # unquoted '}' (which is left for the caller to consume).
      def parse_message(in_plural)
        nodes = []
        text = +''
        quoted = false

        until eof?
          char = peek

          if quoted
            if char == "'"
              if @chars[@pos + 1] == "'"
                text << "'"
                @pos += 2
              else
                quoted = false
                @pos += 1
              end
            else
              text << char
              @pos += 1
            end
            next
          end

          case char
          when "'"
            nxt = @chars[@pos + 1]
            if nxt == "'"
              text << "'"
              @pos += 2
            elsif QUOTABLE.include?(nxt)
              quoted = true
              @pos += 1
            else
              text << "'"
              @pos += 1
            end
          when '{'
            flush_text(nodes, text)
            @pos += 1
            nodes << parse_argument(in_plural)
          when '}'
            break
          when '#'
            if in_plural
              flush_text(nodes, text)
              nodes << NUMBER_NODE
            else
              text << char
            end
            @pos += 1
          else
            text << char
            @pos += 1
          end
        end

        flush_text(nodes, text)
        nodes
      end

      def parse_argument(in_plural)
        skip_whitespace
        name = parse_name
        skip_whitespace

        if peek == '}'
          @pos += 1
          return [:arg, name]
        end

        error("expected ',' or '}' after argument name #{name.inspect}") unless peek == ','
        @pos += 1
        skip_whitespace
        kind = parse_name
        skip_whitespace
        error("expected ',' after #{kind.inspect}") unless peek == ','
        @pos += 1

        case kind
        when 'select' then parse_branches(:select, name, in_plural)
        when 'plural' then parse_branches(:plural, name, true)
        else error("unknown argument type #{kind.inspect}, expected 'select' or 'plural'")
        end
      end

      def parse_branches(kind, name, in_plural)
        branches = {}

        loop do
          skip_whitespace
          error("expected a selector or '}' to close the #{kind} for #{name.inspect}") if eof?
          break if peek == '}'

          key = parse_selector(kind)
          error("duplicate selector #{key.inspect} in the #{kind} for #{name.inspect}") if branches.key?(key)
          skip_whitespace
          error("expected '{' to open the #{key.inspect} branch") unless peek == '{'
          @pos += 1
          branches[key] = parse_message(in_plural)
          error("expected '}' to close the #{key.inspect} branch") unless peek == '}'
          @pos += 1
        end

        @pos += 1 # consume the closing '}' of the argument
        error("the #{kind} for #{name.inspect} requires an 'other' branch") unless branches.key?(:other)
        [kind, name, branches]
      end

      def parse_selector(kind)
        if peek == '='
          error("exact match selectors ('=N') are only allowed in plural arguments") unless kind == :plural
          @pos += 1
          start = @pos
          @pos += 1 while !eof? && peek =~ /\d/
          error("expected a number after '='") if @pos == start
          @chars[start...@pos].join.to_i
        else
          name = parse_name
          if kind == :plural && !PLURAL_CATEGORIES.include?(name.to_sym)
            error("unknown plural category #{name.inspect}, expected one of #{PLURAL_CATEGORIES.map(&:inspect).join(', ')} or '=N'")
          end
          name.to_sym
        end
      end

      def parse_name
        error('expected a name') if eof? || peek !~ NAME_START
        start = @pos
        @pos += 1 while !eof? && peek =~ NAME_CHAR
        @chars[start...@pos].join
      end

      def skip_whitespace
        @pos += 1 while !eof? && peek =~ /\s/
      end

      def flush_text(nodes, text)
        nodes << [:text, text.dup].freeze unless text.empty?
        text.clear
      end

      def peek
        @chars[@pos]
      end

      def eof?
        @pos >= @chars.length
      end

      def error(detail)
        raise SyntaxError.new(@pattern, @pos, detail)
      end
    end
  end
end
