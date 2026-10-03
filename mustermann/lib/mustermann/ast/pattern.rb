# frozen_string_literal: true
require 'mustermann/ast/parser'
require 'mustermann/ast/boundaries'
require 'mustermann/ast/compiler'
require 'mustermann/ast/transformer'
require 'mustermann/ast/validation'
require 'mustermann/ast/template_generator'
require 'mustermann/ast/param_scanner'
require 'mustermann/ast/rename'
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
        ast = @ast ||= self.class.ast_cache.fetch([@string, options]) do
          ast   = parse(@string, pattern: self)
          ast &&= transform(ast)
          ast &&= set_boundaries(ast, string: @string)
          validate(ast)
        end
        @param_converters ||= Mustermann.dedup(scan_params(ast, options)) if ast
        ast
      end

      # All AST-based pattern implementations support renaming captures.
      #
      # @example (see Mustermann::Pattern#rename)
      # @param (see Mustermann::Pattern#rename)
      # @return (see Mustermann::Pattern#rename)
      # @raise (see Mustermann::Pattern#rename)
      # @see Mustermann::Pattern#rename
      def rename(mapping = {}, **kwargs)
        mapping = mapping.merge(kwargs) unless kwargs.empty?
        return self if mapping.empty?
        mapping = mapping.to_h { |name, new_name| [name.to_s, new_name.to_s] }
        return self if (names & mapping.keys).empty?

        ast = Rename.rename(to_ast, mapping)
        validate(ast)

        copy = self.class.allocate
        copy.send(:initialize_rebound, self, ast, mapping)
        copy
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

      # @!visibility private
      def param_converters
        @param_converters ||= Mustermann.dedup(scan_params(to_ast, options))
      end

      # @api private
      def identity_params?(params)
        param_converters.empty? && super
      end

      # @!visibility private
      # @see Mustermann::Pattern#rebound?
      def rebound? = !!@rebound

      # Initializes a fresh instance as a copy of the given pattern with a
      # rebound AST. Shared state (string, options) is reused, all state
      # derived from the AST is compiled from the new tree.
      # @!visibility private
      def initialize_rebound(source, ast, mapping)
        @string     = source.instance_variable_get(:@string)
        @uri_decode = source.uri_decode
        @options    = Mustermann.dedup(rebind_options(source.options || EMPTY_HASH, mapping))
        @ast        = ast
        @rebound    = true
        compile_regexps(**@options.except(:cache))
        reset_caches(@options.fetch(:cache, true))
      end

      # Hands capture constraints on to the new capture names, without
      # touching the constraints themselves.
      # @!visibility private
      def rebind_options(options, mapping)
        capture = options[:capture]
        return options unless capture.is_a? Hash
        rebound = capture.to_h { |name, constraint| [mapping.fetch(name.to_s, name.to_s).to_sym, constraint] }
        options.merge(capture: rebound)
      end

      private :compile, :parse, :transform, :validate, :generate_templates, :param_converters, :scan_params, :set_boundaries,
        :initialize_rebound, :rebind_options
    end
  end
end
