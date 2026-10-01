# frozen_string_literal: true
module Mustermann
  # Class for pattern objects that are a concatenation of other patterns.
  # @see Mustermann::Pattern#+
  class Concat < Composite
    # Mixin for patterns to support native concatenation.
    # @!visibility private
    module Native
      # @see Mustermann::Pattern#+
      # @!visibility private
      def +(other)
        other &&= Mustermann.new(other, type: :identity, **options)
        if (patterns = look_ahead(other)) && !patterns.empty?
          concat = (self + patterns.inject(:+))
          concat + other.patterns.slice(patterns.length..-1).inject(:+)
        else
          native, opts = native_concat(other)
          native ? self.class.new(native, **options, **opts.to_h) : super
        end
      end

      # @!visibility private
      def look_ahead(other)
        return unless other.is_a?(Concat)
        other.patterns.take_while(&method(:native_concat?))
      end

      # @!visibility private
      def native_concat(other)
        "#{self}#{other}" if native_concat?(other)
      end

      # @!visibility private
      def native_concat?(other)
        other.class == self.class and other.options == options
      end

      private :native_concat, :native_concat?
    end

    # Should not be used directly.
    # @!visibility private
    def initialize(*, **)
      super
      AST::Validation.validate(combined_ast) if respond_to? :expand
    end

    # @see Mustermann::Composite#operator
    # @return [Symbol] always :+
    def operator
      :+
    end

    # @see Mustermann::Pattern#===
    def ===(string)
      !!split_match(string, full: true)
    end

    # @see Mustermann::Pattern#match
    def match(string)
      split_match(string, full: true)
    end

    # @see Mustermann::Pattern#params
    def params(string)
      split_match(string, full: true)&.params
    end

    # @see Mustermann::Pattern#peek_size
    def peek_size(string)
      split_match(string)&.to_s&.size
    end

    # @see Mustermann::Pattern#peek_match
    #
    # The individual patterns are matched against the string in order.
    # When an earlier pattern greedily consumes characters that a later
    # pattern needs, shorter splits of the earlier pattern are tried.
    # Among all viable splits, the one with the longest segments from
    # left to right is chosen.
    def peek_match(string)
      split_match(string)
    end

    # @see Mustermann::Pattern#peek_params
    def peek_params(string)
      match = split_match(string)
      [match.params, match.to_s.size] if match
    end

    # (see Mustermann::Pattern#expand)
    def expand(behavior = nil, values = {})
      raise NotImplementedError, 'expanding not supported' unless respond_to? :expand
      @expander ||= Mustermann::Expander.new(self) { combined_ast }
      @expander.expand(behavior, values)
    end

    # (see Mustermann::Pattern#to_templates)
    def to_templates
      raise NotImplementedError, 'template generation not supported' unless respond_to? :to_templates
      @to_templates ||= patterns.inject([EMPTY_STRING]) { |list, pattern| list.product(pattern.to_templates).map(&:join) }.uniq
    end

    # @!visibility private
    def respond_to_special?(method)
      method = :to_ast if method.to_sym == :expand
      patterns.all? { |p| p.respond_to?(method) }
    end

    # Searches for a viable way of splitting the input between the patterns.
    # Splits are explored from left to right, always trying the longest
    # match for the current pattern first. This allows a pattern to back
    # off when it greedily consumed characters that a later pattern needs,
    # while empty matches are allowed to end the search.
    #
    # @param full [Boolean] require the whole string to be consumed
    # @return [Mustermann::Match, nil] match built from one successful split,
    #   so that its params and captures never mix in values from failed candidates
    # @!visibility private
    def split_match(string, full: false)
      search(string, 0, 0, [], full)
    end

    # used by {#split_match} to recursively explore possible splits
    # @!visibility private
    def search(string, index, offset, parts, full)
      pattern = patterns[index]

      if pattern.nil?
        return if full and offset < string.size
        return build_match(string, offset, parts)
      end

      remaining = string[offset, string.size - offset]
      return if remaining.nil?

      # a prefix is a viable split point if the pattern matches it in
      # full; they are checked longest first so that, among all viable
      # splits, each segment is as long as the left-to-right order allows.
      # the empty prefix is included so patterns that can match nothing
      # may end the split.
      remaining.size.downto(0) do |size|
        next unless part = pattern.match(size.zero? ? EMPTY_STRING : remaining[0, size])
        result = search(string, index + 1, offset + size, parts + [part], full)
        return result if result
      end

      nil
    end

    # combines the matches of one successful split into a single match
    # @!visibility private
    def build_match(string, consumed, parts)
      params         = {}
      named_captures = {}
      captures       = []

      parts.each do |part|
        params.merge!(part.params)
        named_captures.merge!(part.named_captures)
        captures.concat(part.captures)
      end

      matched    = string[0, consumed]
      post_match = string[consumed, string.size - consumed]
      Match.new(self, string, matched:, params:, captures:, named_captures:, post_match:)
    end

    # generates one big AST from all patterns
    # will not check if patterns support AST generation
    # @!visibility private
    def combined_ast
      payload = patterns.map { |p| AST::Node[:group].new(p.to_ast.payload) }
      AST::Node[:root].new(payload)
    end

    private :combined_ast, :split_match, :search, :build_match
  end
end
