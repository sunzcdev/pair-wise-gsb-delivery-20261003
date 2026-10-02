# frozen_string_literal: true

require_relative 'errors/invalid_projection'

module Blueprinter
  # A Projection describes the subset of output fields requested for a single
  # render. Projection keys are expressed using output field names (the `name`
  # option), so renamed fields and renamed associations are addressed by their
  # rendered name.
  #
  # Projections are per-render objects: they are never stored on a Blueprint or
  # on any view, so reusing the same Blueprint with different projections never
  # mutates view definitions or the compiled view cache.
  class Projection
    Node = Struct.new(:name, :children, :nested, keyword_init: true) do
      def leaf?
        !nested
      end
    end

    attr_reader :include_identifier

    # Builds a root Projection from a user-provided field spec.
    #
    # @param spec [Array<Symbol, String, Hash>, Symbol, String, Hash, nil]
    #   requested fields. Hash values may specify a nested projection for an
    #   association.
    # @param include_identifier [Boolean] whether identifier fields are kept
    #   when they are not explicitly requested; cascades to nested projections.
    def self.build(spec = nil, include_identifier: true)
      return new(:unprojected, include_identifier, []) if spec.nil?

      new(parse_spec(spec), include_identifier, [])
    end

    def initialize(nodes = :unprojected, include_identifier = true, path = [])
      @nodes = nodes == :unprojected ? nil : (nodes || {})
      @include_identifier = include_identifier
      @path = path
    end

    def projected?
      !@nodes.nil?
    end

    def selected?(field_name)
      return true unless projected?

      @nodes.key?(field_name.to_sym)
    end

    def node_for(field_name)
      @nodes[field_name.to_sym]
    end

    def child_for(field_name)
      node = node_for(field_name)
      return nil unless node

      children = node.leaf? ? :unprojected : (node.children || {})
      self.class.new(children, @include_identifier, [*@path, node.name])
    end

    # Validates this projection against a Blueprint and view. Nodes whose
    # association blueprint is statically known are validated recursively;
    # dynamic (Proc) blueprints are validated at render time, once the
    # associated object is available.
    #
    # @param blueprint [Class<Blueprinter::Base>]
    # @param view_name [Symbol]
    def validate!(blueprint, view_name)
      return unless projected?

      view_collection = blueprint.view_collection
      fields_by_name = view_collection.fields_for(view_name).to_h { |field| [field.name, field] }
      excluded_names = view_collection.excluded_field_names_for(view_name)

      @nodes.each_value do |node|
        validate_node(node, blueprint, view_name, fields_by_name, excluded_names)
      end
    end

    class << self
      private

      def parse_spec(spec)
        case spec
        when Hash
          spec.each_with_object({}) do |(key, value), nodes|
            name = key.to_sym
            validate_key!(name, spec)
            nodes[name] = build_node(name, value)
          end
        when Array
          spec.each_with_object({}) do |entry, nodes|
            case entry
            when Hash
              parse_spec(entry).each do |name, node|
                if nodes.key?(name)
                  raise ArgumentError, "Invalid field projection: field #{name.inspect} is listed more than once"
                end

                nodes[name] ||= node
              end
            else
              name = entry.to_sym
              validate_key!(name, spec)
              if nodes.key?(name)
                raise ArgumentError, "Invalid field projection: field #{name.inspect} is listed more than once"
              end

              nodes[name] = Node.new(name: name, children: nil, nested: false)
            end
          end
        when Symbol, String
          name = spec.to_sym
          validate_key!(name, spec)
          { name => Node.new(name: name, children: nil, nested: false) }
        else
          raise ArgumentError,
                "Invalid field projection: expected an Array or Hash of field names, got #{spec.inspect}"
        end
      end

      def build_node(name, value)
        case value
        when nil, true
          Node.new(name: name, children: nil, nested: false)
        when false
          raise ArgumentError, "Invalid field projection: #{name.inspect} cannot be set to false"
        when Hash
          children = parse_spec(value)
          Node.new(name: name, children: children, nested: true)
        when Array
          children = parse_spec(value)
          Node.new(name: name, children: children, nested: true)
        else
          raise ArgumentError,
                "Invalid field projection for #{name.inspect}: expected nil, true, an Array, or a Hash, got #{value.inspect}"
        end
      end

      def validate_key!(name, source)
        return if name.is_a?(Symbol) && name.length.positive?

        raise ArgumentError, "Invalid field projection entry: #{source.inspect}"
      end
    end

    private

    def validate_node(node, blueprint, view_name, fields_by_name, excluded_names)
      path = [*@path, node.name]

      if excluded_names.include?(node.name)
        raise Errors::InvalidProjection,
              "Field '#{format_path(path)}' is excluded from the '#{view_name}' view"
      end

      field = fields_by_name[node.name]
      unless field
        raise Errors::InvalidProjection,
              "Unknown field '#{format_path(path)}' for '#{blueprint.name || blueprint}'"
      end

      return if node.leaf?

      unless field.options[:association]
        raise Errors::InvalidProjection,
              "Field '#{format_path(path)}' is not an association and cannot have a nested projection"
      end

      nested_blueprint = field.options[:blueprint]
      return if nested_blueprint.is_a?(Proc)

      nested_view = field.options[:view] || :default
      child_for(node.name).validate!(nested_blueprint, nested_view)
    end

    def format_path(path)
      path.map(&:to_s).join('.')
    end
  end
end
