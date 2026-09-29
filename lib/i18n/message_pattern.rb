# frozen_string_literal: true

module I18n
  # MessagePattern is an explicitly opt-in message format for translations
  # that need more structure than plain "%{name}" interpolation, e.g. texts
  # that select wording by both a count and a gender:
  #
  #   pattern = I18n::MessagePattern.parse(
  #     "{gender, select, " \
  #       "male{{count, plural, one{He has one message} other{He has # messages}}} " \
  #       "female{{count, plural, one{She has one message} other{She has # messages}}} " \
  #       "other{{count, plural, one{They have one message} other{They have # messages}}}}"
  #   )
  #   pattern.render(:gender => :female, :count => 3) # => "She has 3 messages"
  #
  # Plain translation strings are never treated as message patterns. A
  # pattern only comes into play when you explicitly parse one (or store an
  # I18n::MessagePattern as translation data / pass +message_pattern: true+
  # to a backend that includes I18n::Backend::MessagePattern).
  #
  # == Syntax
  #
  #   message     := (text | escape | placeholder)*
  #   escape      := "\" <any character>          # \{ \} \\ produce literal { } \
  #   placeholder := "{" name "}"                                # parameter
  #                | "{" name "," "select" "," branches "}"      # select by value
  #                | "{" name "," "plural" "," branches "}"      # select by plural category
  #   branches    := (key "{" message "}")+       # messages may nest placeholders
  #   key         := identifier                   # select
  #                | identifier | "=" number      # plural
  #
  # * Whitespace is allowed around names, commas, keywords and branch keys.
  # * Both +select+ and +plural+ require an +other+ branch as the default.
  # * +plural+ branches are matched in this order: an exact "=n" branch, then
  #   the plural category of the value (see MessagePattern.plural_rule), then
  #   +other+.
  # * Inside a +plural+ branch, "#" renders the numeric value itself.
  #   Outside a plural branch "#" is literal text.
  # * A "{" that does not start a valid placeholder is a syntax error;
  #   escape literal braces as "\{" and "\}".
  #
  # == Evaluation
  #
  # Parsing produces an immutable tree which is cached per source string
  # (see .parse and .clear_cache). Rendering only walks the branches that
  # are actually selected: placeholders in unselected branches are never
  # resolved, so missing keys or callable values there have no effect.
  #
  # Values that respond to +call+ are invoked (with the values hash) when -
  # and only when - they are rendered, mirroring I18n.interpolate.
  class MessagePattern
    # Raised when a message pattern cannot be parsed. The error message
    # points at the exact position of the problem:
    #
    #   I18n::MessagePattern.parse("{count, plural, one{x}")
    #   # => I18n::MessagePattern::SyntaxError:
    #   #    unexpected end of input, expected a branch key or `}` at line 1, column 23
    #   #    {count, plural, one{x}
    #   #                      ^
    class SyntaxError < I18n::ArgumentError
      attr_reader :source, :position, :line, :column

      def initialize(source, position, message)
        @source = source
        @position = position
        @line = source[0...position].count("\n") + 1
        line_start = position.zero? ? 0 : (source.rindex("\n", position - 1) || -1) + 1
        @column = position - line_start + 1
        line_end = source.index("\n", position) || source.length
        snippet = source[line_start...line_end]
        super("#{message} at line #{@line}, column #{@column}\n#{snippet}\n#{' ' * (@column - 1)}^")
      end
    end

    module Nodes # :nodoc:
      class Text
        def initialize(text)
          @text = text
        end

        def evaluate(_values, _number)
          @text
        end
      end

      class Parameter
        def initialize(name, source)
          @name = name
          @source = source
        end

        def evaluate(values, _number)
          MessagePattern.resolve_value(@name, values, @source).to_s
        end
      end

      class NumberSign
        def evaluate(_values, number)
          number.to_s
        end
      end

      class Select
        def initialize(name, branches, source)
          @name = name
          @branches = branches
          @source = source
        end

        def evaluate(values, number)
          value = MessagePattern.resolve_value(@name, values, @source)
          branch = @branches[value.to_s] || @branches['other']
          branch.map { |node| node.evaluate(values, number) }.join
        end
      end

      class Plural
        def initialize(name, exact, categories, source)
          @name = name
          @exact = exact
          @categories = categories
          @source = source
        end

        def evaluate(values, _number)
          value = MessagePattern.resolve_value(@name, values, @source)
          branch = @exact[value] ||
                   @categories[MessagePattern.plural_rule.call(value).to_s] ||
                   @categories['other']
          branch.map { |node| node.evaluate(values, value) }.join
        end
      end
    end

    # Recursive descent parser for the message pattern syntax. :nodoc:
    class Parser
      NAME = /[A-Za-z0-9_]+/
      CLDR_CATEGORIES = %w[zero one two few many other].freeze

      def initialize(source)
        @source = source
        @pos = 0
        @plural_depth = 0
      end

      def parse
        nodes = parse_message
        error("unexpected `}`") unless eof?
        nodes
      end

      private

      def parse_message
        nodes = []
        text = +''

        until eof?
          char = current
          case char
          when '}'
            break
          when '\\'
            advance
            error('dangling escape, expected a character after `\`') if eof?
            text << current
            advance
          when '{'
            nodes << Nodes::Text.new(text) unless text.empty?
            text = +''
            nodes << parse_placeholder
          when '#'
            if @plural_depth > 0
              nodes << Nodes::Text.new(text) unless text.empty?
              text = +''
              nodes << Nodes::NumberSign.new
              advance
            else
              text << char
              advance
            end
          else
            text << char
            advance
          end
        end

        nodes << Nodes::Text.new(text) unless text.empty?
        nodes
      end

      def parse_placeholder
        expect('{')
        skip_whitespace
        name = scan_name || error('expected a placeholder name')
        skip_whitespace

        case current
        when '}'
          advance
          Nodes::Parameter.new(name, @source)
        when ','
          advance
          skip_whitespace
          type = scan_name || error("expected `select` or `plural`")
          unless %w[select plural].include?(type)
            error("unknown placeholder type `#{type}`, expected `select` or `plural`")
          end
          skip_whitespace
          expect(',', "expected `,` after `#{type}`")
          skip_whitespace
          parse_branches(name, type)
        when nil
          error('unexpected end of input, expected `,` or `}`')
        else
          error("unexpected `#{current}`, expected `,` or `}`")
        end
      end

      def parse_branches(name, type)
        exact = {}
        branches = {}

        loop do
          skip_whitespace
          error("unexpected end of input, expected a branch key or `}`") if eof?
          break if current == '}'

          key_pos = @pos
          key =
            if type == 'plural' && current == '='
              advance
              number = scan_number || error('expected a number after `=`')
              number.to_i
            else
              scan_name || error("unexpected `#{current}`, expected a branch key")
            end

          if type == 'plural' && key.is_a?(String) && !CLDR_CATEGORIES.include?(key)
            error("unknown plural category `#{key}`, expected one of #{CLDR_CATEGORIES.join(', ')} or `=n`", key_pos)
          end

          skip_whitespace
          expect('{', "expected `{` to open the `#{key}` branch")
          @plural_depth += 1 if type == 'plural'
          message = parse_message
          @plural_depth -= 1 if type == 'plural'
          error('unexpected end of input, expected `}`') if eof?
          advance # consume `}` closing the branch

          target = key.is_a?(Integer) ? exact : branches
          error("duplicate branch `#{key}`", key_pos) if target.key?(key)
          target[key] = message
        end

        advance # consume `}` closing the placeholder

        unless branches.key?('other')
          error("missing required `other` branch for `#{name}`")
        end

        if type == 'select'
          Nodes::Select.new(name, branches, @source)
        else
          Nodes::Plural.new(name, exact, branches, @source)
        end
      end

      def scan_name
        match = @source.match(NAME, @pos)
        return nil unless match && match.begin(0) == @pos
        @pos = match.end(0)
        match[0]
      end

      def scan_number
        match = @source.match(/[0-9]+/, @pos)
        return nil unless match && match.begin(0) == @pos
        @pos = match.end(0)
        match[0]
      end

      def skip_whitespace
        advance while !eof? && current =~ /\s/
      end

      def expect(char, message = "expected `#{char}`")
        error(message) if eof? || current != char
        advance
      end

      def error(message, position = @pos)
        raise SyntaxError.new(@source, position, message)
      end

      def current
        @source[@pos]
      end

      def advance
        @pos += 1
      end

      def eof?
        @pos >= @source.length
      end
    end

    # The default plural rule follows the English categories: `one` when the
    # value is exactly 1, `other` otherwise. Assign your own callable (e.g.
    # one backed by CLDR rules) to support other locales:
    #
    #   I18n::MessagePattern.plural_rule = ->(count) { count.zero? ? :zero : :other }
    DEFAULT_PLURAL_RULE = ->(count) { count == 1 ? :one : :other }

    @plural_rule = DEFAULT_PLURAL_RULE
    @cache = Concurrent::Map.new

    class << self
      attr_accessor :plural_rule

      # Parses +source+ into a MessagePattern. Results are cached by source
      # string, so repeated lookups of the same pattern do not re-parse.
      def parse(source)
        @cache.compute_if_absent(source) { new(source) }
      end

      # Clears the parse cache (mainly useful for tests).
      def clear_cache
        @cache.clear
      end

      def cache_size
        @cache.size
      end

      # Resolves a placeholder value against the given values hash. Symbol
      # keys are tried first, then String keys. Callable values are invoked
      # with the values hash. Missing keys are delegated to
      # I18n.config.missing_interpolation_argument_handler, just like
      # I18n.interpolate does.
      def resolve_value(name, values, source)
        key = name.to_sym
        key = name unless values.key?(key)
        value = if values.key?(key)
                  values[key]
                else
                  return I18n.config.missing_interpolation_argument_handler.call(key, values, source)
                end
        value.respond_to?(:call) ? value.call(values) : value
      end
    end

    attr_reader :source

    def initialize(source)
      @source = source
      @nodes = Parser.new(source).parse
      freeze
    end

    # Renders the pattern with the given values. Only the selected branches
    # are evaluated.
    def render(values = {})
      @nodes.map { |node| node.evaluate(values, nil) }.join
    end
    alias_method :call, :render
  end
end
