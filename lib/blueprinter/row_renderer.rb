# frozen_string_literal: true

require 'blueprinter/errors/invalid_row_path'
require 'blueprinter/helpers/type_helpers'

module Blueprinter
  # @api private
  #
  # Expands rendered Blueprint data into a flat table. Each rendered object contributes one row
  # per element of a single declared collection association; every other association along a
  # column path must resolve to a plain object ({Hash}), otherwise an error is raised.
  class RowRenderer
    include TypeHelpers

    BLANK_ROW = :blank_row
    OMIT = :omit

    # @param blueprint [Class<Blueprinter::Base>]
    # @param columns [Array<Symbol, String, Array>] ordered output-alias paths
    # @param collection [Symbol, String, Array, nil] output-alias path of the single collection to expand
    # @param options [Hash] render options (e.g. :view)
    def initialize(blueprint:, columns:, collection:, options: {})
      @blueprint = blueprint
      @options = options
      @view_name = options[:view] || :default
      @collection_path = parse_path(collection, allow_nil: true)
      @column_paths = columns.map { |column| parse_path(column) }

      raise ArgumentError, 'columns must not be empty' if @column_paths.empty?

      @on_empty_collection = validate_on_empty_collection(options)

      validate_paths!
    end

    # @param object [Object, Array]
    # @return [Hash{Symbol => Object}] the ordered column names and expanded rows
    def render(object)
      rendered = @blueprint.hashify(
        object,
        view_name: @view_name,
        local_options: render_options
      )

      rendered_objects = array_like?(object) ? rendered : [rendered]
      rows = rendered_objects.flat_map do |rendered_object|
        rows_for(rendered_object)
      end

      { columns: column_names, rows: rows }
    end

    private

    def column_names
      @column_paths.map { |segments| segments.join('.') }
    end

    def render_options
      @options.except(:view, :on_empty_collection)
    end

    def validate_on_empty_collection(options)
      on_empty = options.fetch(:on_empty_collection, BLANK_ROW)
      return on_empty if [BLANK_ROW, OMIT].include?(on_empty)

      raise ArgumentError, "on_empty_collection must be :#{BLANK_ROW} or :#{OMIT}"
    end

    def validate_paths!
      validator = PathValidator.new(blueprint: @blueprint, view_name: @view_name)
      validator.validate_collection_path!(@collection_path) if @collection_path
      @column_paths.each { |path| validator.validate_column_path!(path, @collection_path) }
    end

    def rows_for(rendered_object)
      return [build_row(rendered_object, nil)] unless @collection_path

      elements = resolve_collection(rendered_object)

      if elements.nil? || elements.empty?
        return [] if @on_empty_collection == OMIT

        return [build_row(rendered_object, nil)]
      end

      elements.map { |element| build_row(rendered_object, element) }
    end

    def build_row(rendered_object, element)
      @column_paths.map do |path|
        resolve_cell(path, rendered_object, element)
      end
    end

    # Returns the rendered collection array, an empty collection/nil marker, or raises when the
    # declared path runs through a second collection.
    def resolve_collection(rendered_object)
      node = rendered_object
      @collection_path.each_with_index do |segment, index|
        return nil if node.nil?

        node = descend!(node, segment, @collection_path[0..index])
        if index < @collection_path.length - 1
          return nil if node.nil?

          raise_invalid_path!(@collection_path, 'expected an object association') unless node.is_a?(Hash)
        end
      end

      return nil if node.nil?

      unless array_like?(node)
        raise_invalid_path!(@collection_path, 'does not reference a collection association')
      end

      node
    end

    def resolve_cell(path, rendered_object, element)
      if @collection_path && path[0...@collection_path.length] == @collection_path
        suffix = path[@collection_path.length..]
        return element if suffix.empty?

        dig(element, suffix, path)
      else
        dig(rendered_object, path, path)
      end
    end

    # Walks a rendered hash following the given output aliases. An array encountered before the
    # end of the path is always an undeclared collection, since the only collection allowed to
    # expand was supplied separately.
    def dig(node, segments, full_path)
      segments.each_with_index do |segment, index|
        return nil if node.nil?

        node = descend!(node, segment, full_path)
        if array_like?(node)
          reason = index < segments.length - 1 ? 'crosses an undeclared collection' : 'is an undeclared collection'
          raise_invalid_path!(full_path, reason)
        end
      end
      node
    end

    def descend!(node, segment, full_path)
      raise_invalid_path!(full_path, 'expected an object but found a scalar') unless node.is_a?(Hash)

      # A missing key means the field was hidden by an :if/:unless condition or by
      # :exclude_if_nil; static validation already proved the alias belongs to the view.
      node[segment]
    end

    def raise_invalid_path!(segments, reason)
      raise Errors::InvalidRowPath, "invalid row path '#{segments.join('.')}': #{reason}"
    end

    def parse_path(path, allow_nil: false)
      return nil if path.nil? && allow_nil

      segments =
        case path
        when Array then path.flat_map { |part| part.to_s.split('.') }
        when Symbol, String then path.to_s.split('.')
        else
          raise ArgumentError, "invalid path #{path.inspect}"
        end

      segments = segments.map(&:to_sym).reject { |segment| segment.to_s.empty? }
      raise ArgumentError, 'paths must not be empty' if segments.empty?

      segments
    end

    # @api private
    #
    # Statically verifies column and collection paths against the Blueprint view graph so that
    # typos are reported even when the inspected data is empty. Dynamic (Proc) blueprints and
    # keys injected by transformers cannot be inspected and are treated leniently.
    class PathValidator
      def initialize(blueprint:, view_name:)
        unless blueprint.view_collection.view?(view_name)
          raise Errors::UnknownView, "View '#{view_name}' is not defined"
        end

        @root_blueprint = blueprint
        @root_view = view_name
      end

      def validate_collection_path!(path)
        follow_associations(@root_blueprint, @root_view, path, path)
      end

      def validate_column_path!(path, collection_path)
        if collection_path && path[0...collection_path.length] == collection_path
          suffix = path[collection_path.length..]
          return if suffix.empty?

          element_blueprint, element_view = follow_associations(
            @root_blueprint, @root_view, collection_path, path
          )
          walk(element_blueprint, element_view, suffix, path)
        else
          walk(@root_blueprint, @root_view, path, path)
        end
      end

      private

      # Follows a path whose every segment must be an association and returns the blueprint
      # that renders the final segment's elements.
      def follow_associations(blueprint, view_name, segments, full_path)
        segments.each do |segment|
          field = field_for(blueprint, view_name, segment)
          raise invalid(full_path, "unknown association '#{segment}'") unless field

          next_blueprint, next_view = static_target(field, full_path, segment)
          blueprint, view_name = next_blueprint, next_view
        end
        [blueprint, view_name]
      end

      # Verifies intermediate segments are associations and that the leaf is a declared field.
      # The leaf is accepted when a transformer may inject it. Once a dynamic (Proc) blueprint
      # is reached, deeper keys cannot be inspected statically, so validation stops there.
      def walk(blueprint, view_name, segments, full_path)
        return if blueprint.nil?

        segments[0...-1].each do |segment|
          field = field_for(blueprint, view_name, segment)
          raise invalid(full_path, "unknown field or association '#{segment}'") unless field

          next_blueprint, next_view = static_target(field, full_path, segment)
          return if next_blueprint.nil?

          blueprint, view_name = next_blueprint, next_view
        end

        leaf = segments.last
        field = field_for(blueprint, view_name, leaf)
        if field.nil?
          return unless strict_leaf?(blueprint, view_name)

          raise invalid(full_path, "unknown field or association '#{leaf}'")
        end

        return unless field.options[:association]

        raise invalid(full_path, "'#{leaf}' is an association, not a field")
      end

      # Unknown leaf keys are only tolerated when a transformer may inject them.
      def strict_leaf?(blueprint, view_name)
        blueprint.view_collection.transformers(view_name).empty?
      end

      # @return [Array(Class, Symbol)] the target blueprint and view
      # @return [Array(nil, nil)] when the field uses a dynamic (Proc) blueprint
      def static_target(field, full_path, segment)
        unless field.options[:association]
          raise invalid(full_path, "'#{segment}' is not an association")
        end

        target = field.options[:blueprint]
        return [nil, nil] if target.is_a?(Proc)

        [target, field.options[:view] || :default]
      end

      def field_for(blueprint, view_name, name)
        blueprint.view_collection.fields_for(view_name).find { |f| f.name == name }
      end

      def invalid(segments, reason)
        Errors::InvalidRowPath.new("invalid row path '#{segments.join('.')}': #{reason}")
      end
    end
  end
end
