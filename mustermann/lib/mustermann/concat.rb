# frozen_string_literal: true
module Mustermann
  # Class for pattern objects that are a concatenation of other patterns.
  #
  # Matching is done by splitting the input string between the patterns.
  # If a pattern further to the left consumes too much (or too little) for
  # the remaining patterns to match, it backtracks and tries a different
  # split. When multiple splits are possible, the first one found scanning
  # from left to right wins, with each pattern preferring its longest match.
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
      !!split(string, full: true)
    end

    # @see Mustermann::Pattern#match
    def match(string)
      build_match(string, split(string, full: true))
    end

    # @see Mustermann::Pattern#params
    def params(string)
      return unless parts = split(string, full: true)
      parts.inject({}) { |params, part| params.merge!(part.params) }
    end

    # @see Mustermann::Pattern#peek_size
    def peek_size(string)
      split(string)&.sum { |part| part.to_s.size }
    end

    # @see Mustermann::Pattern#peek_match
    def peek_match(string)
      build_match(string, split(string))
    end

    # @see Mustermann::Pattern#peek_params
    def peek_params(string)
      return unless parts = split(string)
      params = parts.inject({}) { |hash, part| hash.merge!(part.params) }
      [params, parts.sum { |part| part.to_s.size }]
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

    # Finds a way to split +string+ among the patterns, so that every pattern
    # matches its share. Patterns further to the left try their longest match
    # first, backtracking to shorter matches as long as some split exists that
    # lets all following patterns match, too. With +full+, only splits
    # consuming the entire string are considered successful.
    #
    # Returns an Array of Mustermann::Match objects (one per pattern) for the
    # winning split, or nil if no split exists. Only the captures of the
    # winning split are returned, failed candidates are discarded.
    # @!visibility private
    def split(string, full: false)
      split_from(0, string, full, [])
    end

    # @!visibility private
    def split_from(index, string, full, parts)
      return parts if index == patterns.length && (!full || string.empty?)

      if pattern = patterns[index]
        each_match_size(pattern, string) do |size|
          next unless part = pattern.match(string[0, size])
          parts.push(part)
          return parts if split_from(index + 1, string[size..-1], full, parts)
          parts.pop
        end
      end

      nil
    end

    # Yields every length at which +pattern+ matches the beginning of
    # +string+, from longest to shortest. Also yields 0 if the pattern
    # matches the empty string, so empty matches terminate the enumeration
    # instead of causing an endless loop.
    # @!visibility private
    def each_match_size(pattern, string)
      string.size.downto(0) do |size|
        yield size if pattern === string[0, size]
      end
    end

    # builds a single Match object from the matches of a successful split
    # @!visibility private
    def build_match(string, parts)
      return unless parts

      params         = {}
      captures       = []
      named_captures = {}

      parts.each do |part|
        params.merge!(part.params)
        named_captures.merge!(part.named_captures)
        captures.concat(part.captures)
      end

      matched    = string[0, parts.sum { |part| part.to_s.size }]
      post_match = string[matched.size..-1]
      Match.new(self, string, matched:, params:, captures:, named_captures:, post_match:)
    end

    # generates one big AST from all patterns
    # will not check if patterns support AST generation
    # @!visibility private
    def combined_ast
      payload = patterns.map { |p| AST::Node[:group].new(p.to_ast.payload) }
      AST::Node[:root].new(payload)
    end

    private :combined_ast, :split, :split_from, :each_match_size, :build_match
  end
end
