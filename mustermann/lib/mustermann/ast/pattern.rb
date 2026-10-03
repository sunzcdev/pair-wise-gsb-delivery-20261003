# frozen_string_literal: true
require 'mustermann/ast/parser'
require 'mustermann/ast/boundaries'
require 'mustermann/ast/compiler'
require 'mustermann/ast/transformer'
require 'mustermann/ast/validation'
require 'mustermann/ast/template_generator'
require 'mustermann/ast/param_scanner'
require 'mustermann/regexp_based'
require 'mustermann/expander'
require 'mustermann/equality_map'

module Mustermann
  # @see Mustermann::AST::Pattern
  module AST
    # Superclass for pattern styles that parse an AST from the string pattern.
    # @abstract
    class Pattern < Mustermann::RegexpBased
      supported_options :capture, :except, :greedy, :space_matches_plus

      extend Forwardable, SingleForwardable
      single_delegate on: :parser, suffix: :parser
      instance_delegate %i[parser compiler transformer validation template_generator param_scanner boundaries] => 'self.class'
      instance_delegate parse: :parser, transform: :transformer, validate: :validation,
        generate_templates: :template_generator, scan_params: :param_scanner, set_boundaries: :boundaries
      
      # @api private
      def self.ast_cache
        @ast_cache ||= EqualityMap.new
      end

      # @api private
      # @return [#parse] parser object for pattern
      # @!visibility private
      def self.parser
        return Parser if self == AST::Pattern
        const_set :Parser, Class.new(superclass.parser) unless const_defined? :Parser, false
        const_get :Parser
      end

      # @api private
      # @return [#compile] compiler object for pattern
      # @!visibility private
      def self.compiler
        Compiler
      end

      # @api private
      # @return [#set_boundaries] translator making sure start and stop is set on all nodes
      # @!visibility private
      def self.boundaries
        Boundaries
      end

      # @api private
      # @return [#transform] transformer object for pattern
      # @!visibility private
      def self.transformer
        Transformer
      end

      # @api private
      # @return [#validate] validation object for pattern
      # @!visibility private
      def self.validation
        Validation
      end

      # @api private
      # @return [#generate_templates] generates URI templates for pattern
      # @!visibility private
      def self.template_generator
        TemplateGenerator
      end

      # @api private
      # @return [#scan_params] param scanner for pattern
      # @!visibility private
      def self.param_scanner
        ParamScanner
      end

      # @!visibility private
      def compile(**options)
        options[:except] &&= parse options[:except]
        compiler.compile(to_ast, **options)
      rescue CompileError => error
        raise error.class, "#{error.message}: #{@string.inspect}", error.backtrace
      end

      # Returns a regexp that matches strings excluded by the +except+ option,
      # or +nil+ if no +except+ constraint was given. Used by the trie matcher
      # to filter out excluded strings at leaf nodes.
      # @return [Regexp, nil]
      # @!visibility private
      def except_regexp
        return unless except_str = options[:except]
        @except_regexp ||= begin
          source = compiler.new.translate(parse(except_str), no_captures: true, **options.except(:except))
          Mustermann.dedup(Regexp.new("\\A#{source}\\z"))
        end
      end

      # Internal AST representation of pattern.
      # @!visibility private
      def to_ast
        ast = self.class.ast_cache.fetch([@string, options]) do
          ast   = parse(@string, pattern: self)
          ast &&= transform(ast)
          ast &&= set_boundaries(ast, string: @string)
          validate(ast)
        end
        @param_converters ||= Mustermann.dedup(scan_params(ast, options)) if ast
        ast
      end

      # All AST-based pattern implementations support expanding.
      #
      # @example (see Mustermann::Pattern#expand)
      # @param (see Mustermann::Pattern#expand)
      # @return (see Mustermann::Pattern#expand)
      # @raise (see Mustermann::Pattern#expand)
      # @see Mustermann::Pattern#expand
      # @see Mustermann::Expander
      def expand(behavior = nil, values = EMPTY_HASH)
        @expander ||= Mustermann::Expander.new(self)
        @expander.expand(behavior, values)
      end

      # All AST-based pattern implementations support generating templates.
      #
      # @example (see Mustermann::Pattern#to_templates)
      # @param (see Mustermann::Pattern#to_templates)
      # @return (see Mustermann::Pattern#to_templates)
      # @see Mustermann::Pattern#to_templates
      def to_templates
        @to_templates ||= Mustermann.dedup(generate_templates(to_ast))
      end

      # @!visibility private
      # @see Mustermann::Pattern#map_param
      def map_param(key, value)
        return super unless param_converters.include? key
        converted = super
        converted.nil? ? converted : param_converters[key][converted]
      end

      # All AST-based pattern implementations support renaming captures.
      #
      # The new pattern is generated by rewriting the capture names in the
      # pattern string, guided by the AST, so literal text (like the contents
      # of constraints) is left untouched. Options constraining captures (like
      # the +capture+ option) are carried over to the new names.
      #
      # @example (see Mustermann::Pattern#rename)
      # @param (see Mustermann::Pattern#rename)
      # @return (see Mustermann::Pattern#rename)
      # @raise (see Mustermann::Pattern#rename)
      # @see Mustermann::Pattern#rename
      def rename(map = nil, **kwargs)
        renames = normalize_renames(map, kwargs)
        return self if renames.empty?

        unknown = renames.keys - names
        raise ArgumentError, "no capture named #{unknown.first.inspect} in #{inspect}" unless unknown.empty?

        renamed = names.map { |name| renames.fetch(name, name) }
        if duplicate = renamed.tally.find { |_, count| count > 1 }
          raise ArgumentError, "renaming would result in duplicate capture name #{duplicate.first.inspect}"
        end

        renames.each_value do |new_name|
          next if new_name.match?(/\A[a-z_]\w*\z/)
          raise ArgumentError, "invalid capture name #{new_name.inspect}"
        end

        self.class.new(renamed_string(renames), **renamed_options(renames))
      end

      # @!visibility private
      def renamed_string(renames)
        edits   = []
        applied = []

        each_ast_node(to_ast) do |node|
          next unless node.is_a?(Node::Capture) and node.payload.is_a?(String)
          new_name = renames[node.payload] or next
          offset   = @string[node.start...node.stop].index(node.payload) or next
          edits   << [node.start + offset, node.payload.length, new_name]
          applied << node.payload
        end

        missing = renames.keys - applied
        raise ArgumentError, "cannot rename capture #{missing.first.inspect} in #{inspect}" unless missing.empty?

        edits.sort_by!(&:first)
        edits.reverse_each.with_object(@string.dup) do |(start, length, new_name), string|
          string[start, length] = new_name
        end
      end

      # @!visibility private
      def renamed_options(renames)
        renamed = options ? options.dup : {}
        if renamed[:capture].is_a? Hash
          renamed[:capture] = renamed[:capture].to_h do |key, value|
            new_name = renames[key.to_s]
            key      = key.is_a?(Symbol) ? new_name.to_sym : new_name if new_name
            [key, value]
          end
        end
        renamed
      end

      # @!visibility private
      def each_ast_node(node, &block)
        yield node
        case payload = node.payload
        when Array then payload.each { |entry| each_ast_node(entry, &block) if entry.is_a?(Node) }
        when Node  then each_ast_node(payload, &block)
        end
        each_ast_node(node.head, &block) if node.is_a?(Node::WithLookAhead)
      end

      # @!visibility private
      def param_converters
        @param_converters ||= Mustermann.dedup(scan_params(to_ast, options))
      end

      # @api private
      def identity_params?(params)
        param_converters.empty? && super
      end

      private :compile, :parse, :transform, :validate, :generate_templates, :param_converters, :scan_params, :set_boundaries,
        :renamed_string, :renamed_options, :each_ast_node
    end
  end
end
