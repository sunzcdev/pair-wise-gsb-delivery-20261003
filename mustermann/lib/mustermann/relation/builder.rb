# frozen_string_literal: true
require 'uri'
require 'mustermann/ast/converters'

module Mustermann
  class Relation
    # Raised internally when a pattern uses constructs the symbolic analysis cannot
    # model reliably. Carries a human readable reason.
    #
    # @api private
    class UnknownConstruct < Mustermann::Error
    end

    # Translates a parsed Mustermann AST into the symbolic {IR} representation.
    #
    # Only constructs the analyzer can model exactly are accepted. Custom capture
    # constraints (Regexp, unknown POSIX classes, etc.) raise {UnknownConstruct}
    # so the relation is reported as unknown rather than guessed.
    #
    # @api private
    class Builder
      # Exact ASCII memberships of the POSIX bracket classes accepted as capture
      # constraints. Non-ASCII codepoints are intentionally excluded; any evidence
      # path relying on them is rejected against the real matchers (see Relation).
      POSIX_RANGES = {
        word:   ((?0..?9).to_a + (?A..?Z).to_a + (?a..?z).to_a + [?_]).map(&:ord),
        digit:  (?0..?9).map(&:ord),
        alpha:  ((?A..?Z).to_a + (?a..?z).to_a).map(&:ord),
        alnum:  ((?0..?9).to_a + (?A..?Z).to_a + (?a..?z).to_a).map(&:ord),
        lower:  (?a..?z).map(&:ord),
        upper:  (?A..?Z).map(&:ord),
        space:  ["\t", "\n", "\v", "\f", "\r", " "].map(&:ord),
        blank:  ["\t", " "].map(&:ord),
        punct:  ((33..47).to_a + (58..64).to_a + (91..96).to_a + (123..126).to_a),
        xdigit: ((?0..?9).to_a + (?a..?f).to_a + (?A..?F).to_a).map(&:ord),
        print:  (32..126).to_a,
        graph:  (33..126).to_a,
        cntrl:  ((0..31).to_a + [127])
      }.freeze

      def self.build(pattern)
        new(pattern).build
      end

      def initialize(pattern)
        @pattern = pattern
        @options = pattern.options
        @reasons = []
      end

      # @return [Array(IR::Node, Array<String>)] node and unknown reasons (empty if known)
      def build
        node = build_pattern(@pattern)
        [node, reasons]
      rescue UnknownConstruct
        [nil, reasons]
      end

      attr_reader :reasons

      private

      def build_pattern(pattern)
        case pattern
        when AST::Pattern then build_ast(pattern)
        when Identity     then build_identity(pattern)
        when Composite    then build_composite(pattern)
        when Concat       then build_concat(pattern)
        else unknown("#{pattern.class} patterns cannot be analyzed structurally")
        end
      end

      def build_ast(pattern)
        node = translate(pattern.to_ast, pattern, uri_decode: pattern.uri_decode)
        apply_except(node, pattern)
      end

      # Translates an AST node or array of AST nodes into IR.
      def translate(node, pattern, uri_decode:)
        case node
        when Array
          translate_sequence(node, pattern, uri_decode:)
        when AST::Node::Root
          translate_sequence(node.payload, pattern, uri_decode:)
        when AST::Node::Separator
          separator(node.payload, uri_decode:)
        when AST::Node::Char
          character(node.payload, uri_decode:)
        when AST::Node::Group
          node.payload.is_a?(Array) ? translate_sequence(node.payload, pattern, uri_decode:) :
            translate(node.payload, pattern, uri_decode:)
        when AST::Node::Optional
          IR::F.opt(translate(node.payload, pattern, uri_decode:))
        when AST::Node::Union
          IR::F.alt(*node.payload.map { |child| translate(child, pattern, uri_decode:) })
        when AST::Node::Splat, AST::Node::NamedSplat
          splat(pattern, uri_decode:)
        when AST::Node::Capture
          capture(node, pattern, uri_decode:)
        when AST::Node::WithLookAhead
          raise Mustermann::Error, 'with_look_ahead must be translated within a sequence'
        else
          unknown("#{node.class.name.split('::').last} constructs are not supported")
        end
      end

      # Translates an array of siblings, giving a with_look_ahead node access to the
      # suffix and everything following it as its stopping continuation.
      def translate_sequence(nodes, pattern, uri_decode:)
        parts = []
        index = 0
        while index < nodes.size
          child = nodes[index]
          if child.is_a?(AST::Node::WithLookAhead)
            # The node absorbs everything following it as its stopping continuation.
            after_nodes = nodes[index + 1..]
            parts << with_look_ahead(child, pattern, uri_decode:, after_nodes:)
            break
          else
            parts << translate(child, pattern, uri_decode:)
            index += 1
          end
        end
        IR::F.seq(*parts)
      end

      # A literal path separator is never matched through its percent encoding.
      def separator(string, uri_decode:)
        IR::F.char(IR::Predicate.singleton(string.ord))
      end

      # Literal characters match their raw form and all URI encodings, mirroring
      # AST::Compiler#encoded.
      def character(char, uri_decode:)
        return IR::F.char(IR::Predicate.singleton(char.ord)) unless uri_decode
        alternatives = representations(char).map do |representation|
          IR::F.seq(*representation.each_char.map { |c| IR::F.char(IR::Predicate.singleton(c.ord)) })
        end
        IR::F.alt(*alternatives)
      end

      def representations(char)
        if char == ' ' && @options.fetch(:space_matches_plus, true)
          return representations('+') + representations_for(' ')
        end
        representations_for(char)
      end

      def representations_for(char)
        escaped = uri_parser.escape(char, /./)
        [char, escaped.upcase, escaped.downcase].uniq
      end

      # Splats compile to a non-greedy dot-star, which accepts every string.
      def splat(pattern, uri_decode:)
        IR::F.star(IR::F.char(IR::Predicate.dot))
      end

      # Ordinary captures compile to [^/?#]+ unless a constraint option applies.
      def capture(node, pattern, uri_decode:)
        # AST capture nodes with an inline constraint set by the parser/renderer.
        if node.constraint || node.qualifier || node.convert
          unknown("inline capture constraints cannot be analyzed")
        end
        constraint = lookup_capture(node.name)
        if constraint.nil?
          # Default capture compiles to [^/?#]+.
          IR::F.plus(default_capture_body)
        else
          node, qualified = build_constraint(constraint)
          qualified ? IR::F.plus(node) : node
        end
      end

      def default_capture_body
        IR::F.char(IR::Predicate.all_except(?/.ord, ??.ord, ?#.ord))
      end

      def lookup_capture(name)
        capture = @options[:capture]
        return nil if capture.nil?
        capture.is_a?(Hash) ? capture[name.to_sym] : capture
      end

      # @return [Array(Node, Boolean)] the constraint node and whether it needs the
      #   + qualifier (POSIX classes and global symbol constraints do)
      def build_constraint(constraint)
        case constraint
        when nil then nil
        when String
          [IR::F.seq(*constraint.each_char.map { |c| character(c, uri_decode: true) }), false]
        when Symbol
          symbol_constraint(constraint)
        when Class
          [class_constraint(constraint), false]
        when Array
          qualified = constraint.any? { |entry| entry.is_a?(Symbol) && !converters.key?(entry) }
          [IR::F.alt(*constraint.map { |entry| build_constraint(entry).first }), qualified]
        when Regexp
          unknown("custom Regexp capture constraints cannot be analyzed")
        else
          unknown("capture constraint #{constraint.inspect} is not supported")
        end
      end

      def symbol_constraint(symbol)
        converter = converters[symbol]
        return [regexp_constraint(symbol, converter.first), false] if converter
        [posix_constraint(symbol), true]
      end

      # POSIX bracket classes used by constraints like capture: :word. The compiler
      # qualifies these with +, handled by the caller.
      def posix_constraint(symbol)
        positions = POSIX_RANGES[symbol] or
          unknown("capture constraint #{symbol.inspect} is not a built-in converter")
        IR::F.char(IR::Predicate.new(positions))
      end

      def class_constraint(klass)
        converter = converters[klass.name]
        unknown("no built-in converter for #{klass}") unless converter
        regexp_constraint(klass, converter.first)
      end

      def converters
        AST.const_get(:CONVERTERS)
      end

      # Built-in converter regexps are trusted and parseable with {RegexpParser}.
      def regexp_constraint(label, regexp)
        unless regexp.is_a?(Regexp)
          raise UnknownConstruct, "capture constraint #{label.inspect} has no regular expression"
        end
        RegexpParser.parse(regexp.source)
      end

      # Models the WithLookAhead nodes produced by the AST transformer for greedy
      # captures followed by optional suffixes.
      def with_look_ahead(node, pattern, uri_decode:, after_nodes: [])
        head_ast = node.head
        body     = head_ast.is_a?(AST::Node::Capture) ? capture(head_ast, pattern, uri_decode:) :
                   translate(head_ast, pattern, uri_decode:)
        suffix_ir, inner_ir = optional_parts(node.payload, pattern, uri_decode:)
        rest     = translate_sequence(after_nodes, pattern, uri_decode:)
        after    = IR::F.seq(suffix_ir, rest)
        terminator = node.at_end ? :line_end : IR::F.char(IR::Predicate.singleton(?/.ord))
        if node.at_end
          IR::F.guarded(body, inner_ir, :line_end, after)
        else
          IR::F.guarded(body, inner_ir, terminator, after)
        end
      end

      # Returns [full suffix IR, suffix inner IR], unwrapping a single Optional.
      def optional_parts(payload, pattern, uri_decode:)
        payload = payload.first if payload.is_a?(Array) && payload.size == 1
        full    = translate(payload, pattern, uri_decode:)
        inner   = payload.is_a?(AST::Node::Optional) ?
          translate(payload.payload, pattern, uri_decode:) : full
        [full, inner]
      end

      def apply_except(node, pattern)
        except = @options[:except]
        return node unless except
        except_options = pattern.options.except(:except)
        except_pattern = pattern.class.new(except, **except_options)
        except_node, = Builder.new(except_pattern).build
        raise UnknownConstruct, "except pattern #{except.inspect} cannot be analyzed" if except_node.nil?
        # The compiler emits (?!except\Z), which removes exactly the strings that
        # fully match the except pattern from the node language.
        IR::F.inter(node, IR::F.not(except_node))
      end

      # Identity patterns compare against the URI-unescaped pattern string. Every
      # character matches its raw and percent-encoded forms (the raw '/' separator
      # is, like other characters, also available in encoded form).
      def build_identity(pattern)
        string = pattern.to_s
        IR::F.seq(*string.each_char.map { |char| identity_character(char) })
      end

      def identity_character(char)
        escaped = uri_parser.escape(char, /./)
        alternatives = [char, escaped.upcase, escaped.downcase].uniq.map do |representation|
          IR::F.seq(*representation.each_char.map { |c| IR::F.char(IR::Predicate.singleton(c.ord)) })
        end
        IR::F.alt(*alternatives)
      end

      def build_composite(pattern)
        children = pattern.patterns.map { |child| build_pattern(child) }
        case pattern.operator
        when :|
          IR::F.alt(*children)
        when :&
          IR::F.inter(*children)
        when :^
          left, right = children
          IR::F.alt(IR::F.inter(left, IR::F.not(right)), IR::F.inter(IR::F.not(left), right))
        else
          unknown("composite operator #{pattern.operator.inspect} is not supported")
        end
      end

      def build_concat(pattern)
        IR::F.seq(*pattern.patterns.map { |child| build_pattern(child) })
      end

      def unknown(message)
        @reasons ||= []
        @reasons << message
        raise UnknownConstruct, message
      end

      def uri_parser
        @@uri_parser ||= URI::RFC2396_Parser.new
      end
    end
  end
end
