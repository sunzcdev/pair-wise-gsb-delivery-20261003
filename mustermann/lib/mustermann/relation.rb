# frozen_string_literal: true
require 'mustermann/relation/nfa'
require 'mustermann/relation/predicates'
require 'mustermann/relation/regexp_compiler'
require 'mustermann/relation/capture_language'
require 'mustermann/relation/builder'
require 'mustermann/relation/analyzer'
require 'mustermann/relation/witness'

module Mustermann
  # Static analysis of how the *languages* of two patterns relate.
  #
  # Unlike testing a finite batch of URLs, this answers the questions over the
  # full set of strings each pattern can match:
  #
  #   * {#intersect?} – is there any request both patterns would match?
  #   * {#subset?}    – does every request matched by the first pattern also
  #                     match the second?
  #
  # When the answer is informative, concrete evidence is provided as a real
  # path string that can be handed back to the existing matching interface
  # (+pattern === path+ / +pattern.match(path)+) for verification:
  #
  #   * {#witness}        for an intersection,
  #   * {#counterexample} when the first pattern is *not* contained in the
  #                        second.
  #
  # Patterns are compared by the language their options produce, never by
  # their source text, so two patterns written with different encodings or
  # syntax are still compared correctly.
  #
  # When a pattern uses a construct the analyzer cannot model reliably (for
  # example a custom +Regexp+ constraint or the +except:+ option), the
  # corresponding answers are +nil+ ("unknown") and {#reason} explains why.
  #
  # @example
  #   require 'mustermann'
  #
  #   relation = Mustermann.relation('/users/:id', '/users/:name')
  #   relation.intersect? # => true
  #   relation.witness    # => "/users/example"
  #   relation.subset?    # => true
  #
  #   Mustermann.relation('/a', '/b').intersect? # => false
  module Relation
    # Outcome of comparing two patterns.
    #
    # @!attribute intersect?
    #   @return [Boolean, nil] whether the languages share a string
    #     (+nil+ if it could not be determined)
    # @!attribute subset?
    #   @return [Boolean, nil] whether the first language is contained in the
    #     second (+nil+ if it could not be determined)
    # @!attribute witness
    #   @return [String, nil] a string matched by both patterns
    # @!attribute counterexample
    #   @return [String, nil] a string matched by the first but not the second
    # @!attribute reason
    #   @return [String, nil] why the analysis is incomplete
    Result = Struct.new(:intersect?, :subset?, :witness, :counterexample,
      :reason, :left_pattern, :right_pattern, keyword_init: true) do
      # @return [Boolean] true when at least one question is undecided
      def unknown?
        intersect?.nil? || subset?.nil?
      end

      # @return [Boolean] true when the languages share no string
      def disjoint?
        intersect? == false
      end

      # Verifies the returned evidence against the real matchers.  Useful as a
      # self-check and in tests; always returns true when no evidence exists.
      #
      # @return [Boolean]
      def verified?
        if witness
          return false unless left_pattern === witness && right_pattern === witness
        end
        if counterexample
          return false unless left_pattern === counterexample &&
            !(right_pattern === counterexample)
        end
        true
      end

      def inspect
        "#<Mustermann::Relation intersect?=#{intersect?.inspect} " \
          "subset?=#{subset?.inspect}#{reason ? " reason=#{reason.inspect}" : EMPTY_STRING}>"
      end
    end

    # @overload relation(left, right, **options)
    #   Analyzes two patterns.
    #
    #   @param [String, Mustermann::Pattern] left first pattern
    #   @param [String, Mustermann::Pattern] right second pattern
    #   @param [Hash] options used when converting strings to patterns
    #   @return [Result]
    def self.analyze(left, right, **options)
      left_pattern  = coerce(left, **options)
      right_pattern = coerce(right, **options)

      unsupported = unsupported_reason(left_pattern) || unsupported_reason(right_pattern)
      if unsupported
        return Result.new(intersect?: nil, subset?: nil, reason: unsupported,
          left_pattern: left_pattern, right_pattern: right_pattern)
      end

      # Unions of AST patterns are analyzed per alternative; intersections and
      # other compositions would need extra machinery and are reported unknown.
      left_alternatives  = alternatives(left_pattern)
      right_alternatives = alternatives(right_pattern)
      unless left_alternatives && right_alternatives
        return Result.new(intersect?: nil, subset?: nil,
          reason: "pattern composition cannot be analyzed",
          left_pattern: left_pattern, right_pattern: right_pattern)
      end

      outcome =
        begin
          analyze_alternatives(left_alternatives, right_alternatives)
        rescue AnalysisError => error
          error
        end

      if outcome.is_a?(AnalysisError)
        return Result.new(intersect?: nil, subset?: nil, reason: outcome.message,
          left_pattern: left_pattern, right_pattern: right_pattern)
      end

      result = Result.new(
        intersect?:     outcome.intersect?,
        subset?:        outcome.sub_set?,
        witness:        beautify(outcome.witness, outcome,
                                 left_pattern, right_pattern, :both),
        counterexample: beautify(outcome.counterexample, outcome,
                                 left_pattern, right_pattern, :left),
        left_pattern:   left_pattern,
        right_pattern:  right_pattern)

      # Defensive verification: witnesses are generated from the analyzed
      # model, but never report evidence the actual matchers reject.
      unless result.verified?
        return Result.new(intersect?: nil, subset?: nil,
          reason: "internal model disagreement while generating evidence",
          left_pattern: left_pattern, right_pattern: right_pattern)
      end
      result
      end

      def self.coerce(input, **options)
        input.is_a?(Pattern) ? input : Mustermann.new(input, **options)
      end

      def self.unsupported_reason(pattern)
        return nil if pattern.respond_to?(:to_ast)
        return "pattern composition cannot be analyzed" if pattern.is_a?(Composite)
        "pattern type #{pattern.class.name.split('::').last} cannot be analyzed"
      end

      # Returns the list of AST patterns represented by the input, or nil if
      # the composition cannot be flattened to a union.
      def self.alternatives(pattern)
        return [pattern] if pattern.respond_to?(:to_ast)
        if pattern.is_a?(Mustermann::Composite) && pattern.operator == :|
          members = pattern.patterns
          return nil unless members.all? { |member| member.respond_to?(:to_ast) }
          return members.flat_map { |member| alternatives(member) || [] }
        end
        nil
      end

      # Intersection and containment over unions of patterns:
      #   intersect?(L, R) iff exists l in L, r in R with intersect?(l, r)
      #   subset?(L, R)    iff for every l in L exists r in R with subset?(l, r)
      def self.analyze_alternatives(left_list, right_list)
        # Flattening both sides to a single union NFA makes intersection and
        # containment exact over the whole composed language.
        Analyzer.analyze_nfas(union_nfa(left_list), union_nfa(right_list))
      end

      def self.union_nfa(list)
        nfas   = list.map { |pattern| Builder.build(pattern) }
        merged = NFA.new
        nfas.each do |nfa|
          info = merged.merge_from(nfa)
          merged.epsilon(merged.start, info[:start])
        end
        merged
      end

      # Produce a readable witness and make sure it is still accepted by the
      # real matchers; fall back to the raw (always valid) witness otherwise.
      def self.beautify(raw, outcome, left, right, kind)
        return raw unless raw
        pretty = Witness.beautify(raw, outcome.left_nfa, outcome.right_nfa)
        valid =
          case kind
          when :both then left === pretty && right === pretty
          when :left then left === pretty && !(right === pretty)
          end
        valid ? pretty : raw
      end
  end

  # @!scope class
  # Analyzes the relation between two patterns.
  # @see Mustermann::Relation
  # @return [Mustermann::Relation::Result]
  def self.relation(left, right, **options)
    Relation.analyze(left, right, **options)
  end
end
