# frozen_string_literal: true
require 'mustermann/relation/nfa'
require 'mustermann/relation/predicates'
require 'mustermann/relation/regexp_compiler'
require 'mustermann/relation/capture_language'

module Mustermann
  module Relation
    # Raised when a pattern uses a feature the static relation analyzer cannot
    # model reliably.  The message always states the reason.
    class AnalysisError < StandardError
    end

    # Builds an {NFA} that recognizes the same language as an
    # {Mustermann::AST::Pattern}.
    #
    # The structure (sequence, optional, union, repetition) is taken directly
    # from the pattern's transformed AST, while the character-level content of
    # individual captures is taken from the exact same source strings the
    # {Mustermann::AST::Compiler} emits, keeping both models in lockstep.
    #
    # @api private
    class Builder
      # Character class sources, kept identical to AST::Compiler.
      DEFAULT_CAPTURE   = '[^/\\?#]'.freeze
      SIMPLE_VARIABLE   = '[\\w\\-\\.~%]'.freeze
      RESERVED_VARIABLE =
        '[\\w\\-\\.~%\\:\\/\\?#\\[\\]@\\!\\$\\&\'\\(\\)\\*\\+,;=]'.freeze

      # Inline constraint sources emitted by Mustermann's built-in converters
      # (e.g. Flask +int+/+float+).  Kept in sync with the bundled converters.
      BUILT_IN_INLINE_SOURCES = ['\d', '\d*\.?\d+'].freeze

      # Build an NFA for a pattern.
      #
      # @param [Mustermann::AST::Pattern] pattern
      # @return [NFA]
      def self.build(pattern)
        new(pattern).build
      end

      def initialize(pattern)
        @pattern = pattern
        @options = pattern.options
        @graph   = NFA.new
      end

      def build
        ast = @pattern.to_ast
        check_options!
        collect_literal_symbols
        fragment = translate(ast)
        @graph.finish!(fragment)
        @graph
      end

      # Non-ASCII characters appearing literally in the pattern must be part of
      # the analyzer's alphabet or two patterns sharing such a literal could be
      # falsely reported as disjoint.
      def collect_literal_symbols
        source = @pattern.respond_to?(:to_s) ? @pattern.to_s.to_s : EMPTY_STRING
        extras = source.each_char.select { |char| char.ord > 127 }
        @graph.add_extra_symbols(extras)
      end

      private

      # Options that alter matching in ways the builder cannot model from the
      # AST alone.
      def check_options!
        if @options[:except]
          raise AnalysisError, "the except option cannot be analyzed"
        end
      end

      # -- AST translation --------------------------------------------------

      def translate(node)
        case
        when node.is_a?(:root), node.is_a?(:group)
          sequence(node.payload)
        when node.is_a?(:separator)
          raw_literal(node.payload)
        when node.is_a?(:char)
          char(node.payload)
        when node.is_a?(:optional)
          @graph.optional_fragment(translate(node.payload))
        when node.is_a?(:union)
          @graph.union_fragment(node.payload.map { |child| translate(child) })
        when node.is_a?(:splat), node.is_a?(:named_splat)
          splat(node)
        when node.is_a?(:variable)
          variable(node)
        when node.is_a?(:capture)
          capture(node)
        when node.is_a?(:expression)
          expression(node)
        when node.is_a?(:with_look_ahead)
          look_ahead(node)
        else
          raise AnalysisError, "unsupported AST node #{node.type.inspect}"
        end
      end

      def sequence(nodes)
        parts = Array(nodes).map { |child| translate(child) }
        @graph.concat_fragment(*parts)
      end

      # -- Literals ---------------------------------------------------------

      # Literal that never percent-expands (path separators compiled by
      # AST::Compiler are plain +Regexp.escape+).
      def raw_literal(string)
        pieces = string.each_char.map do |c|
          @graph.char_fragment(Predicates.exact(c))
        end
        @graph.concat_fragment(*pieces)
      end

      # A literal character, honoring URI decoding (the raw character matches
      # as well as its percent-encoded forms).
      def char(value)
        representations =
          if @options.fetch(:uri_decode, true)
            Predicates.encodings(value,
              space_matches_plus: @options.fetch(:space_matches_plus, true))
          else
            [value]
          end
        # A multi-character encoding (e.g. "%2F") consumes three raw chars.
        alternatives = representations.map do |representation|
          pieces = representation.each_char.map do |piece|
            @graph.char_fragment(Predicates.exact(piece))
          end
          @graph.concat_fragment(*pieces)
        end
        @graph.union_fragment(alternatives)
      end

      # -- Captures ---------------------------------------------------------

      def capture(node)
        kind, payload = capture_body(node)
        case kind
        when :regexp
          compile_source(qualified(payload, node.qualifier))
        when :literal
          # String capture constraints are fixed text: the compiler expands
          # every character through the URI encoding helper and adds the
          # capture's own qualifier afterwards.
          base = literal_text(payload)
          apply_literal_qualifier(base, node.qualifier)
        end
      end

      def literal_text(string)
        pieces = string.each_char.map { |c| char(c) }
        @graph.concat_fragment(*pieces)
      end

      def apply_literal_qualifier(fragment, qualifier)
        return fragment if qualifier.nil?
        # Literal captures are produced by +from_string+, which itself adds no
        # +++ qualifier; a node qualifier (e.g. from a Flask converter) repeats
        # the whole fixed text.
        case qualifier
        when '+' then @graph.repeat_fragment(fragment, 1, nil)
        when '*' then @graph.repeat_fragment(fragment, 0, nil)
        when ''  then fragment
        when /\A\{(\d+)(?:,(\d*))?\}\z/
          min = Integer($1)
          max = $2.nil? ? min : ($2.empty? ? nil : Integer($2))
          @graph.repeat_fragment(fragment, min, max)
        else raise AnalysisError, "unsupported capture qualifier #{qualifier.inspect}"
        end
      end

      def splat(_node)
        # Splats are always non-greedy ".*?", which has the same language as
        # ".*" (everything except a newline).
        @graph.repeat_fragment(@graph.char_fragment(Predicates::DOT), 0, nil)
      end

      # A +with_look_ahead+ node wraps a greedy capture (the +head+) followed
      # by an optional group (the +tail+) that starts with a literal separator.
      # The compiler emits a per-character negative look-ahead so the greedy
      # capture stops at the last position from which the tail can complete the
      # string.
      #
      # For a default full-segment capture (excluding +/+ +?+ +#+), let +s+ be
      # the tail's leading separator character.  The recognized language after
      # the leading slash is one or more segment characters that are *not* a
      # single +s+ followed by a non-empty +s+-free suffix; a string made only
      # of separators is still accepted.  We model exactly that language.
      def look_ahead(node)
        head = node.head
        tail_parts = optional_payload_parts(node)
        unless head.is_a?(:capture) && tail_parts &&
               (leading = tail_leading_literal(tail_parts))
          raise AnalysisError, "unsupported capture look-ahead structure"
        end

        unless default_capture?(head)
          raise AnalysisError, "unsupported capture look-ahead structure"
        end

        # The tail after the separator must not contain optional parts or
        # unions for this structural model to apply.
        unless tail_after_separator(tail_parts, leading)
          raise AnalysisError, "unsupported capture look-ahead structure"
        end

        build_lookahead_language(leading)
      end

      # Returns the payload array inside the optional group, or nil.
      def optional_payload_parts(node)
        list = node.payload
        return unless list.is_a?(Array) && list.size == 1 && list.first.is_a?(:optional)
        inner = list.first.payload
        inner = inner.payload if inner.is_a?(:group)
        inner.is_a?(Array) ? inner : [inner].compact
      end

      # The literal character introducing the tail (a char or separator),
      # provided it is the first payload element.
      def tail_leading_literal(parts)
        first = parts.first
        return unless first
        if first.is_a?(:char) || first.is_a?(:separator)
          first.payload
        end
      end

      # Nodes following the leading separator literal, translated.
      def tail_after_separator(parts, leading)
        first = parts.first
        return unless first && (first.is_a?(:char) || first.is_a?(:separator)) &&
          first.payload == leading && parts.size >= 2
        rest = parts[1..]
        rest.any? && rest.none? { |node| node.is_a?(:optional) || node.is_a?(:union) }
      end

      # Builds the fragment for one-or-more default-segment characters
      # recognizing every non-empty run except exactly +S [^S]++ (a single
      # leading separator followed by a non-empty separator-free suffix).
      def build_lookahead_language(separator)
        any     = -> { compile_source(DEFAULT_CAPTURE) }
        non_sep = -> { compile_source('[^/\\?#%s]' % Regexp.escape(separator)) }
        sep     = -> { raw_literal(separator) }

        # Accepted non-empty runs are those that are not "S [^S]+":
        #  1. starts with a non-separator            : [^S] C*
        #  2. starts with S but has another S present: S [^S]* S C*
        #  3. is exactly one or more separators      : S+
        starts_non_sep = @graph.concat_fragment(
          non_sep.call, @graph.repeat_fragment(any.call, 0, nil))
        two_separators = @graph.concat_fragment(
          sep.call,
          @graph.repeat_fragment(non_sep.call, 0, nil),
          sep.call,
          @graph.repeat_fragment(any.call, 0, nil))
        only_separators = @graph.repeat_fragment(sep.call, 1, nil)

        @graph.union_fragment([starts_non_sep, two_separators, only_separators])
      end

      # @return [Array(Symbol, Object)] +[:regexp, source]+ or +[:literal, s]+
      def capture_body(node)
        inline = node.constraint
        value  = constraint_value(node)

        return inline_constraint(inline) if inline
        return capture_option(value) if value
        [:regexp, DEFAULT_CAPTURE]
      end

      def default_capture?(node)
        kind, payload = capture_body(node)
        kind == :regexp && payload == DEFAULT_CAPTURE
      end

      def capture_option(value)
        case value
        when String then [:literal, value]
        when Regexp
          raise AnalysisError, "custom Regexp constraint cannot be analyzed"
        when Array
          sources = value.map { |entry| capture_option(entry) }
          if sources.any? { |kind, _| kind == :literal }
            raise AnalysisError, "mixed capture constraints cannot be analyzed"
          end
          [:regexp, "(?:#{sources.map { |_, source| source }.join('|')})"]
        when Hash
          raise AnalysisError, "nested capture constraints cannot be analyzed"
        else
          source = CaptureLanguage.source(value)
          [:regexp, source || DEFAULT_CAPTURE]
        end
      end

      # Inline constraints are produced either by Mustermann's built-in
      # converters (simple, fully understood fragments) or by user-supplied
      # converters (arbitrary Regexp objects).  A Regexp is only accepted when
      # it matches one of the built-in converter sources; anything else leaves
      # the analysis "unknown".  String constraints are parsed directly.
      def inline_constraint(constraint)
        case constraint
        when String
          [:literal, constraint]
        when Regexp
          source = constraint.source
          if BUILT_IN_INLINE_SOURCES.include?(source)
            [:regexp, source]
          else
            raise AnalysisError, "custom capture constraint cannot be analyzed"
          end
        else
          raise AnalysisError, "custom capture constraint cannot be analyzed"
        end
      end

      def constraint_value(node)
        capture = @options[:capture]
        case capture
        when Hash then capture[node.name.to_sym]
        else capture
        end
      end

      # Applies a built-in qualifier to a capture body source, mirroring
      # AST::Compiler::Capture#qualified.
      def qualified(source, qualifier)
        greedy = @options.fetch(:greedy, true)
        suffix = qualifier || (greedy ? '+' : '+?')
        "#{source}#{suffix}"
      end

      # -- Template variables and expressions ------------------------------

      def variable(node)
        @current_variable_name = node.name
        body   = variable_body
        prefix = node.prefix ? "{1,#{Integer(node.prefix)}}" : nil

        core = body + (prefix || variable_quantifier)
        if node.explode
          unit  = @variable_parametric ? parametric(core, node.name) : core
          repeated = "#{unit}(?:#{Regexp.escape(@variable_separator)}#{unit})*"
          compile_source(repeated)
        else
          pattern = @variable_parametric ? parametric(core, node.name) : core
          compile_source(pattern)
        end
      ensure
        @current_variable_name = nil
      end

      def variable_body
        @variable_allow_reserved ? RESERVED_VARIABLE : SIMPLE_VARIABLE
      end

      def variable_quantifier
        @options.fetch(:greedy, true) ? '+' : '+?'
      end

      def parametric(source, name)
        "#{Regexp.escape(name)}(?:=#{source})?"
      end

      def expression(node)
        operator = node.operator
        @variable_allow_reserved = operator.allow_reserved
        @variable_parametric     = operator.parametric
        @variable_separator      = operator.separator

        children = node.payload.dup
        prefix_fragment = nil
        if operator.prefix && !operator.prefix.empty?
          prefix_fragment = raw_literal(operator.prefix)
          # The transformer keeps the prefix separator as the payload's first
          # child; it is already represented by prefix_fragment.
          first = children.first
          if first.is_a?(:separator) && first.payload == operator.prefix
            children.shift
          end
        end
        body = sequence(children)
        @graph.concat_fragment(*[prefix_fragment, body].compact)
      ensure
        @variable_allow_reserved = nil
        @variable_parametric     = nil
        @variable_separator      = nil
      end

      # -- Shared helpers ---------------------------------------------------

      def compile_source(source)
        RegexpCompiler.compile(@graph, source,
          space_matches_plus: @options.fetch(:space_matches_plus, true))
      rescue RegexpCompiler::Unsupported => error
        raise AnalysisError, error.message
      end
    end
  end
end
