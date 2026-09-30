# frozen_string_literal: true

require 'sinatra/base'

module Sinatra
  # = Sinatra::ParamSources
  #
  # Tracks which source a request parameter came from: route captures,
  # the query string or the request body. Applications can declare which
  # sources are allowed for specific parameters and how conflicts across
  # sources (including scalar versus array clashes) are handled.
  #
  # == Usage
  #
  #   require 'sinatra/base'
  #   require 'sinatra/param_sources'
  #
  #   class App < Sinatra::Base
  #     register Sinatra::ParamSources
  #
  #     # only the route capture is accepted for :id
  #     param_source :id, :route
  #
  #     # query string or body accepted, conflicts allowed
  #     param_source :tag, :query, :body, on_conflict: :allow
  #   end
  #
  # Without any declared rules the default +params+ merge behavior is kept
  # unchanged. Once a rule exists for a parameter, violating requests are
  # rejected with 400 Bad Request before the route block is executed.
  #
  # == Parameter sources
  #
  # +route+ :: captures extracted from the path pattern, such as +:id+
  # +query+ :: pairs parsed from the query string
  # +body+  :: form pairs parsed from the request body
  #
  # Repeated plain keys (e.g. +?id=9&id=10+) follow Rack semantics where
  # the last occurrence wins, but every occurrence is remembered through
  # ParamSources::Helpers#param_occurrences and #raw_param_pairs.
  module ParamSources
    # Raised when a parameter violates a declared source rule. It is a
    # Sinatra::BadRequest so the request is answered with 400.
    class SourceViolation < BadRequest
      attr_reader :param_path

      def initialize(message, param_path)
        @param_path = param_path
        super(message)
      end
    end

    SCALAR = :scalar
    ARRAY  = :array
    HASH   = :hash

    SOURCES = %i[route query body].freeze

    # Registry of declared rules, stored as a class instance variable on
    # application classes and copied into subclasses on inheritance.
    class RuleSet
      DEFAULT_POLICY = :reject

      def initialize
        @rules = {}
      end

      def declare(path, *sources, on_conflict: nil, default_policy: DEFAULT_POLICY)
        path  = ParamSources.key_path(path)
        allow = sources.map { |source| ParamSources.source_name(source) }

        if allow.empty?
          raise ArgumentError, "at least one source must be allowed for #{path.inspect}"
        end

        unknown = allow - SOURCES
        unless unknown.empty?
          raise ArgumentError, "unknown parameter source: #{unknown.inspect}"
        end

        policy = on_conflict || default_policy
        unless %i[reject allow prefer].include?(policy)
          raise ArgumentError, "unknown conflict policy: #{policy.inspect}"
        end

        @rules[path] = { allow: allow, on_conflict: policy }
      end

      def [](path)
        @rules[ParamSources.key_path(path)]
      end

      def each(&block)
        @rules.each(&block)
      end

      def with_added_rule(path, sources, options, default_policy: DEFAULT_POLICY)
        copy = self.class.new
        each do |rule_path, rule|
          copy.instance_variable_get(:@rules)[rule_path] = rule.dup
        end
        copy.declare(path, *sources, **options, default_policy: default_policy)
        copy
      end

      def copy_rules
        copy = self.class.new
        each do |rule_path, rule|
          copy.instance_variable_get(:@rules)[rule_path] = rule.dup
        end
        copy
      end
    end

    # Per-request view of the three parameter sources.
    class Sources
      attr_reader :route, :query, :body

      def initialize(context)
        @context = context
        @route   = IndifferentHash.new
        @query   = IndifferentHash.new
        @body    = IndifferentHash.new
      end

      # Raw [key, value] pairs, preserving every occurrence exactly as
      # received, before nested expansion.
      def raw_pairs(source)
        pairs_for(source).map { |key, value| [key, value] }
      end

      # Every raw occurrence of a path, including repeated plain keys.
      def occurrences(path)
        path = ParamSources.key_path(path)
        result = []
        SOURCES.each do |source|
          raw_paths_for(source).each do |raw_path, key, value|
            next unless raw_path == path

            result << { source: source, key: key, value: value }
          end
        end
        result
      end

      # All leaf paths provided by the given source. Array elements are
      # represented by nil segments.
      def key_paths(source)
        leaves.fetch(source).keys
      end

      # Shape (:scalar, :array or :hash) of a path inside one source.
      def shape(source, path)
        shapes = leaves.fetch(source)[ParamSources.key_path(path)]
        return nil unless shapes

        shapes.size == 1 ? shapes.first : shapes
      end

      # Sources that contribute a value at the given path.
      def sources_of(path)
        path = ParamSources.key_path(path)
        SOURCES.select { |source| leaves[source].key?(path) }
      end

      # Validate all declared rules. Raises SourceViolation on the first
      # violation, before the route block is executed.
      def validate!(rules)
        rules.each do |path, rule|
          present = SOURCES.select { |source| leaves[source].key?(path) }
          next if present.empty?

          unauthorized = present - rule[:allow]
          unless unauthorized.empty?
            raise SourceViolation.new(
              "parameter #{ParamSources.path_name(path)} is not allowed from #{unauthorized.join(', ')}",
              path
            )
          end

          next unless present.size > 1

          case rule[:on_conflict]
          when :allow
            next
          when :prefer
            shape_sets = present.map { |source| leaves.fetch(source)[path] }
            has_scalar = shape_sets.any? { |set| set.include?(SCALAR) }
            has_array  = shape_sets.any? { |set| set.include?(ARRAY) }
            next unless has_scalar && has_array

            raise SourceViolation.new(
              "parameter #{ParamSources.path_name(path)} has conflicting scalar/array sources: #{present.join(', ')}",
              path
            )
          else
            raise SourceViolation.new(
              "parameter #{ParamSources.path_name(path)} is provided by multiple sources: #{present.join(', ')}",
              path
            )
          end
        end
      end

      # Replace the route capture tree, used after a route pass.
      def replace_route(params)
        @route = ParamSources.indifferent(params)
        @leaves = nil
        @raw_paths&.clear
      end

      def load_request
        @query = ParamSources.indifferent(@context.request.GET)
        @body  = ParamSources.indifferent(@context.request.POST)
        @leaves = nil
        self
      rescue Rack::Utils::ParameterTypeError, Rack::Utils::InvalidParameterError => error
        raise BadRequest, "Invalid query parameters: #{Rack::Utils.escape_html(error.message)}"
      rescue EOFError => error
        raise BadRequest, "Invalid multipart/form-data: #{Rack::Utils.escape_html(error.message)}"
      end

      private

      def leaves
        @leaves ||= SOURCES.each_with_object({}) do |source, memo|
          tree =
            case source
            when :route then @route
            when :query then @query
            else             @body
            end
          found = {}
          ParamSources.collect_leaves(tree, [], found)
          memo[source] = found
        end
      end

      def pairs_for(source)
        case source
        when :route
          @route.map { |key, value| [key, value] }
        when :query
          Rack::Utils.default_query_parser
               .parse_query_pairs(@context.request.query_string.to_s, '&')
        when :body
          @context.request.form_pairs
        end
      end

      def raw_paths_for(source)
        @raw_paths ||= {}
        @raw_paths[source] ||= pairs_for(source).filter_map do |key, value|
          parsed = ParamSources.split_key_path(key)
          next unless parsed

          [parsed, key, value]
        end
      end
    end

    class << self
      # Split a raw parameter key such as +a[b][]+ into path segments.
      # Array brackets are represented by nil. Malformed keys return nil.
      def split_key_path(name)
        return [] if name.nil? || name.empty?

        segments = []
        if (index = name.index('[', 1))
          root = name[0, index]
          return nil if root.empty?

          segments << root
          rest = name[index..]
        else
          return [name]
        end

        until rest.empty?
          if rest.start_with?('[]')
            segments << nil
            rest = rest[2..]
          elsif rest.start_with?('[')
            close = rest.index(']', 1)
            return nil unless close

            segment = rest[1, close - 1]
            return nil if segment.empty? || segment.include?('[')

            segments << segment
            rest = rest[(close + 1)..]
          else
            return nil
          end
        end

        segments
      end

      # Accept a single key, a dot path (+"a.b"+), an array of segments
      # (+[:a, :b]+) or a bracketed key (+"a[b]"+).
      def key_path(path)
        case path
        when Array
          path.flat_map do |segment|
            segment.nil? ? [nil] : (split_key_path(segment.to_s) || [segment.to_s])
          end
        when Symbol
          key_path(path.to_s)
        when String
          if path.include?('[')
            split_key_path(path) || [path]
          elsif path.include?('.')
            path.split('.')
          else
            [path]
          end
        else
          raise ArgumentError, "unsupported parameter path: #{path.inspect}"
        end
      end

      def path_name(path)
        first, *rest = path
        return first.to_s if rest.empty?

        first.to_s + rest.map { |segment| segment.nil? ? '[]' : "[#{segment}]" }.join
      end

      def source_name(source)
        source = source.to_sym if source.respond_to?(:to_sym)
        case source
        when :path, :captures, :route_params then :route
        when :query_string, :qs               then :query
        when :form, :form_data, :post         then :body
        else source
        end
      end

      def indifferent(value)
        value.is_a?(Hash) && !value.is_a?(IndifferentHash) ? IndifferentHash[value] : value
      end

      # Walk a parsed parameter tree and record the shape of every leaf
      # path. Scalar leaves and the inside of arrays share their path, so
      # a path can have multiple shapes (e.g. one-element arrays).
      def collect_leaves(node, path, found)
        case node
        when Hash
          if node.empty?
            (found[path] ||= []) << HASH unless found[path]&.include?(HASH)
          else
            node.each { |key, child| collect_leaves(child, path + [key.to_s], found) }
          end
        when Array
          (found[path] ||= []) << ARRAY unless found[path]&.include?(ARRAY)
          node.each { |child| collect_leaves(child, path + [nil], found) }
        else
          (found[path] ||= []) << SCALAR unless found[path]&.include?(SCALAR)
        end
      end
    end

    # Class-level DSL mixed into registered applications.
    module ClassMethods
      # Declare the sources allowed for a parameter:
      #
      #   param_source :id, :route
      #   param_source 'filter[name]', :query, :body, on_conflict: :allow
      def param_source(path, *sources, **options)
        @param_source_rules = param_source_rules.with_added_rule(
          path, sources, options,
          default_policy: (default_param_conflict if respond_to?(:default_param_conflict))
        )
      end

      # Default policy used when +on_conflict+ is not given.
      def param_conflict_policy(policy)
        set :default_param_conflict, policy
      end

      # Declared rules for this class, including those inherited from
      # superclasses. Subclass declarations never mutate the parent.
      def param_source_rules
        if defined?(@param_source_rules) && @param_source_rules
          @param_source_rules
        elsif superclass.respond_to?(:param_source_rules)
          superclass.param_source_rules
        else
          RuleSet.new
        end
      end

      def inherited(subclass)
        super
        rules = respond_to?(:param_source_rules) ? param_source_rules : RuleSet.new
        subclass.instance_variable_set(:@param_source_rules, rules.copy_rules)
      end
    end

    # Request-level helpers available inside routes and filters.
    module Helpers
      # Trees kept separate by source.
      def route_params
        param_sources.route
      end

      def query_params
        param_sources.query
      end

      def body_params
        param_sources.body
      end

      def param_sources
        @param_sources ||= build_param_sources
      end

      # Sources contributing to a parameter path, e.g. [:route, :query].
      def param_source_of(path)
        param_sources.sources_of(path)
      end

      def param_from?(source, path)
        param_source_of(path).include?(ParamSources.source_name(source))
      end

      # Every raw occurrence of a path, including repeated query keys.
      def param_occurrences(path)
        param_sources.occurrences(path)
      end

      def raw_param_pairs(source = nil)
        if source
          param_sources.raw_pairs(ParamSources.source_name(source))
        else
          ParamSources::SOURCES.flat_map { |name| param_sources.raw_pairs(name) }
        end
      end

      def raw_param_paths(source)
        param_sources.key_paths(ParamSources.source_name(source))
      end

      # Called by Sinatra::Base#process_route right after route captures
      # are merged into params.
      def capture_param_sources!(params)
        return unless param_sources_enabled?
        return if params.nil? || params.empty?

        if @param_sources
          @param_sources.replace_route(params)
        else
          @param_sources = build_param_sources(params)
        end
      end

      # Called after a candidate route finishes or passes.
      def reset_param_sources!
        return unless param_sources_enabled?
        return if @env['sinatra.error']

        @param_sources&.replace_route({})
      end

      # Called after conditions pass, immediately before the route block.
      def enforce_param_sources!
        return unless param_sources_enabled?

        param_sources.validate!(settings.param_source_rules)
      end

      # Remember the route captures when a route block raised, so error
      # handlers see the same source view.
      def stash_param_sources!(params)
        return unless param_sources_enabled?

        @env['sinatra.error.route_params'] = params
      end

      def restore_param_sources!(_error_params)
        return unless param_sources_enabled?
        return unless @env['sinatra.error.route_params']

        if @param_sources
          @param_sources.replace_route(@env['sinatra.error.route_params'])
        else
          @param_sources = build_param_sources(@env['sinatra.error.route_params'])
        end
      end

      def param_sources_enabled?
        settings.respond_to?(:param_source_rules)
      end

      private

      def build_param_sources(route_params = {})
        ParamSources::Sources.new(self).tap do |sources|
          sources.load_request
          sources.replace_route(route_params) unless route_params.empty?
        end
      end
    end

    def self.registered(base)
      unless base.respond_to?(:default_param_conflict)
        base.set :default_param_conflict, RuleSet::DEFAULT_POLICY
      end
      base.helpers Helpers
      base.extend ClassMethods
    end

    Base.helpers Helpers
    Base.instance_variable_set(:@param_source_rules, RuleSet.new)
  end
end
