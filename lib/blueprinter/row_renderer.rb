# frozen_string_literal: true

require 'blueprinter/errors/invalid_row_path'
require 'blueprinter/errors/unknown_view'
require 'blueprinter/helpers/type_helpers'

module Blueprinter
  # @api private
  #
  # Renders objects into flat, ordered rows by expanding exactly one
  # collection association declared by the caller. Each element of the
  # collection becomes a row; parent-side columns are evaluated once per
  # parent object and repeated across the rows generated from it.
  class RowRenderer
    include TypeHelpers

    BLANK_ROW_STRATEGIES = %i[blank blank_row blank_detail empty].freeze
    OMIT_STRATEGIES = %i[omit skip drop exclude].freeze

    # @param blueprint [Class<Blueprinter::Base>]
    # @param columns [Array<Symbol, String, Array>] ordered column paths
    # @param collection [Symbol, String, Array] the single collection
    #   association path to expand
    # @param view_name [Symbol]
    # @param local_options [Hash]
    # @param on_empty [Symbol, Boolean] how to treat nil/empty collections
    def initialize(blueprint, columns:, collection:, view_name:, local_options:, on_empty: :blank_row)
      @blueprint = blueprint
      @view_name = view_name
      @local_options = local_options
      @on_empty = normalize_on_empty(on_empty)
      @collection_path = self.class.parse_path(collection, :collection)
      @columns = columns.map { |column| Column.new(self.class.parse_path(column, :columns), @collection_path) }

      raise Blueprinter::Errors::InvalidRowPath, 'at least one column must be declared' if @columns.empty?

      validate_columns!
    end

    # @return [Array<Symbol>] the ordered column names
    def column_names
      @columns.map(&:name)
    end

    # @param object [Object, Enumerable] a single object or an array-like
    #   collection of parent objects
    # @return [Array<Array>] one positional array per generated row
    def render(object)
      local_options = @local_options.merge(view: @view_name)
      rendered = @blueprint.hashify(
        object,
        view_name: @view_name,
        local_options:
      )
      parent_hashes = array_like?(object) ? rendered : [rendered]

      parent_hashes.flat_map { |parent_hash| rows_for(parent_hash) }
    end

    # Parses a column path expressed as an Array of aliases or a
    # dot-separated String.
    #
    # @param path [Symbol, String, Array]
    # @param argument [Symbol] name of the argument, used in error messages
    # @return [Array<Symbol>]
    def self.parse_path(path, argument = :path)
      segments =
        case path
        when Array
          path.flat_map { |segment| segment.is_a?(String) || segment.is_a?(Symbol) ? segment.to_s.split('.') : [segment] }
        when String
          path.split('.')
        when Symbol
          path.to_s.split('.')
        else
          raise Blueprinter::Errors::InvalidRowPath,
                "invalid #{argument} path #{path.inspect}: expected an Array, Symbol, or dot-separated String"
        end

      segments = segments.map do |segment|
        case segment
        when Symbol then segment
        when String then segment.to_sym
        else
          raise Blueprinter::Errors::InvalidRowPath,
                "invalid #{argument} path #{path.inspect}: segments must be Symbols or Strings"
        end
      end

      if segments.empty?
        raise Blueprinter::Errors::InvalidRowPath, "invalid #{argument} path #{path.inspect}: path is empty"
      end

      segments
    end

    private

    def normalize_on_empty(on_empty)
      case on_empty
      when *BLANK_ROW_STRATEGIES, true
        :blank_row
      when *OMIT_STRATEGIES, false
        :omit
      else
        raise ArgumentError,
              "invalid on_empty: #{on_empty.inspect}; expected one of " \
              "#{(BLANK_ROW_STRATEGIES + OMIT_STRATEGIES).inspect}"
      end
    end

    # Statically walks the blueprint structure so a misspelled or
    # non-traversable path fails fast. Plain fields end a path; transformers
    # may add keys dynamically, so views with transformers are traversed at
    # runtime only.
    def validate_columns!
      unless @blueprint.view_collection.view?(@view_name)
        raise Blueprinter::Errors::UnknownView, "View '#{@view_name}' is not defined"
      end

      statically_validated = @blueprint.view_collection.transformers(@view_name).empty?
      return unless statically_validated

      @columns.each { |column| validate_path!(column.segments) }
      validate_path!(@collection_path, :collection)
    end

    def validate_path!(segments, label = :column)
      current_blueprint = @blueprint
      current_view = @view_name

      segments.each_with_index do |segment, index|
        field = find_field(current_blueprint, current_view, segment)

        if field.nil?
          raise Blueprinter::Errors::InvalidRowPath,
                "invalid row #{label} path #{segments.map(&:to_s).join('.').inspect}: " \
                "#{segment.inspect} is not a field or association of #{current_blueprint.name || current_blueprint}"
        end

        if index < segments.length - 1 && !field.options[:association]
          raise Blueprinter::Errors::InvalidRowPath,
                "invalid row #{label} path #{segments.map(&:to_s).join('.').inspect}: " \
                "#{segment.inspect} is a field, not an association, and cannot be traversed"
        end

        next if index == segments.length - 1

        nested_blueprint = field.options[:blueprint]
        # A proc-selected blueprint cannot be inspected before render time.
        return if nested_blueprint.is_a?(Proc)

        current_blueprint = nested_blueprint
        current_view = field.options[:view] || :default

        return unless current_blueprint.view_collection.view?(current_view)
      end
    end

    def find_field(blueprint, view_name, name)
      blueprint.view_collection.fields_for(view_name).find { |field| field.name == name }
    end

    def rows_for(parent_hash)
      context, collection_value = walk_to_collection(parent_hash)

      if collection_value.nil? || (array_like?(collection_value) && collection_value.empty?)
        return [] if @on_empty == :omit

        return [blank_row(context)]
      end

      unless array_like?(collection_value)
        raise Blueprinter::Errors::InvalidRowPath,
              "declared collection #{@collection_path.map(&:to_s).join('.').inspect} is not array-like"
      end

      parent_values = parent_column_values(context)

      collection_value.map do |element|
        @columns.map do |column|
          column.parent? ? parent_values[column] : column.detail_value(element)
        end
      end
    end

    # Follows the collection path through the already-rendered parent hash.
    # Crossing any array-like value before the final segment is an implicit
    # cartesian product and is rejected.
    def walk_to_collection(parent_hash)
      context = parent_hash

      @collection_path.each_with_index do |segment, index|
        value = fetch_cell(context, segment)

        if index < @collection_path.length - 1
          if array_like?(value)
            raise Blueprinter::Errors::InvalidRowPath,
                  "invalid collection path: #{segment.inspect} is a collection; " \
                  'only the single declared collection may be expanded'
          end

          context = value
        else
          return [ParentContext.new(parent_hash), value]
        end
      end
    end

    def blank_row(context)
      parent_values = parent_column_values(context)
      @columns.map { |column| column.parent? ? parent_values[column] : nil }
    end

    # Evaluates every parent-side column once for this parent object.
    def parent_column_values(context)
      @columns.each_with_object({}) do |column, values|
        values[column] = column.parent_value(context) if column.parent?
      end
    end

    def fetch_cell(hash, key)
      return nil unless hash.is_a?(Hash)

      hash.fetch(key) { hash.fetch(key.to_s, nil) }
    end

    # View over a single rendered parent hash. Top-level aliases are read once
    # and reused for every generated row; the parent hash itself was produced
    # by a single `hashify` pass, so parent fields are never re-extracted per
    # detail row.
    class ParentContext
      def initialize(parent_hash)
        @parent_hash = parent_hash
        @root_cache = {}
      end

      def root_value(segment)
        @root_cache[segment] ||= cell_from(@parent_hash, segment)
      end

      def cell_from(hash, key)
        return nil unless hash.is_a?(Hash)

        hash.fetch(key) { hash.fetch(key.to_s, nil) }
      end
    end

    # One ordered column path, split into parent-side and detail-side
    # segments relative to the declared collection path.
    class Column
      attr_reader :segments, :name

      def initialize(segments, collection_path)
        @segments = segments
        @name = segments.last
        @is_detail = segments.first(collection_path.length) == collection_path
        @detail_segments = @is_detail ? segments[collection_path.length..] : nil
      end

      def parent?
        !@is_detail
      end

      def parent_value(context)
        traverse(context.root_value(@segments.first), @segments, 1, context)
      end

      def detail_value(element)
        traverse(element, @detail_segments, 0, nil)
      end

      private

      def traverse(start, segments, start_index, context)
        value = start

        (start_index...segments.length).each do |index|
          segment = segments[index]
          value = context ? context.cell_from(value, segment) : cell_from(value, segment)

          if array_like?(value)
            raise Blueprinter::Errors::InvalidRowPath,
                  "invalid column path #{segments.map(&:to_s).join('.').inspect}: " \
                  "collection #{segment.inspect} was not declared for expansion; " \
                  'a second collection cannot be joined into the rows'
          end
        end

        value
      end

      def cell_from(hash, key)
        return nil unless hash.is_a?(Hash)

        hash.fetch(key) { hash.fetch(key.to_s, nil) }
      end

      def array_like?(value)
        Blueprinter.configuration.array_like_classes.any? { |klass| value.is_a?(klass) }
      end
    end
  end
end
