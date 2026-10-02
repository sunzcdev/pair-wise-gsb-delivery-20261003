# frozen_string_literal: true

require 'blueprinter/errors/invalid_root'
require 'blueprinter/errors/invalid_row_path'
require 'blueprinter/errors/meta_requires_root'
require 'blueprinter/errors/unknown_view'
require 'blueprinter/deprecation'
require 'blueprinter/row_renderer'

module Blueprinter
  # Encapsulates the rendering logic for Blueprinter.
  module Rendering
    include TypeHelpers

    # Generates a JSON formatted String represantation of the provided object.
    #
    # @param object [Object] the Object to serialize.
    # @param options [Hash] the options hash which requires a :view. Any
    #   additional key value pairs will be exposed during serialization.
    # @option options [Symbol] :view Defaults to :default.
    #   The view name that corresponds to the group of
    #   fields to be serialized.
    # @option options [Symbol|String] :root Defaults to nil.
    #   Render the json/hash with a root key if provided.
    # @option options [Any] :meta Defaults to nil.
    #   Render the json/hash with a meta attribute with provided value
    #   if both root and meta keys are provided in the options hash.
    #
    # @example Generating JSON with an extended view
    #   post = Post.all
    #   Blueprinter::Base.render post, view: :extended
    #   # => "[{\"id\":1,\"title\":\"Hello\"},{\"id\":2,\"title\":\"My Day\"}]"
    #
    # @return [String] JSON formatted String
    def render(object, options = {})
      jsonify(build_result(object:, options:))
    end

    # Generates a Hash representation of the provided object.
    # Takes a required object and an optional view.
    #
    # @param object [Object] the Object to serialize upon.
    # @param options [Hash] the options hash which requires a :view. Any
    #   additional key value pairs will be exposed during serialization.
    # @option options [Symbol] :view Defaults to :default.
    #   The view name that corresponds to the group of
    #   fields to be serialized.
    # @option options [Symbol|String] :root Defaults to nil.
    #   Render the json/hash with a root key if provided.
    # @option options [Any] :meta Defaults to nil.
    #   Render the json/hash with a meta attribute with provided value
    #   if both root and meta keys are provided in the options hash.
    #
    # @example Generating a hash with an extended view
    #   post = Post.all
    #   Blueprinter::Base.render_as_hash post, view: :extended
    #   # => [{id:1, title: Hello},{id:2, title: My Day}]
    #
    # @return [Hash]
    def render_as_hash(object, options = {})
      build_result(object:, options:)
    end

    # Generates a JSONified hash.
    # Takes a required object and an optional view.
    #
    # @param object [Object] the Object to serialize upon.
    # @param options [Hash] the options hash which requires a :view. Any
    #   additional key value pairs will be exposed during serialization.
    # @option options [Symbol] :view Defaults to :default.
    #   The view name that corresponds to the group of
    #   fields to be serialized.
    # @option options [Symbol|String] :root Defaults to nil.
    #   Render the json/hash with a root key if provided.
    # @option options [Any] :meta Defaults to nil.
    #   Render the json/hash with a meta attribute with provided value
    #   if both root and meta keys are provided in the options hash.
    #
    # @example Generating a hash with an extended view
    #   post = Post.all
    #   Blueprinter::Base.render_as_json post, view: :extended
    #   # => [{"id" => "1", "title" => "Hello"},{"id" => "2", "title" => "My Day"}]
    #
    # @return [Hash]
    def render_as_json(object, options = {})
      build_result(object:, options:).as_json
    end

    # Renders objects into flat, ordered rows suitable for spreadsheet or CSV
    # export. Exactly one collection association is expanded: each of its
    # elements occupies one row, while parent-level columns are repeated on
    # every row generated from the same parent.
    #
    # Column paths use Blueprint output aliases and may cross ordinary
    # (single) object associations. Paths beginning with the declared
    # collection path read fields from the current collection element; all
    # other paths read parent-level values.
    #
    # Fields remain subject to views, :if/:conditions, transformers, and
    # extractors. Conditionally hidden fields render as blank cells rather
    # shifting later columns. Each parent object is rendered once.
    #
    # This method only builds the data structure; it does not write files.
    #
    # @param object [Object, Enumerable] a single object or an array-like
    #   collection of parent objects
    # @param columns [Array<Symbol, String, Array>] ordered column paths,
    #   e.g. `[:id, :customer_name, ['items', :sku'], 'items.price']`. May be
    #   passed positionally or as the `columns:` keyword.
    # @param collection [Symbol, String, Array] the single collection
    #   association path to expand, e.g. `:items` or `:order.items`. May be
    #   passed positionally or as the `collection:` keyword.
    # @param options [Hash] rendering options
    # @option options [Symbol] :view Defaults to :default. View used for both
    #   parent objects and nested associations.
    # @option options [Symbol, Boolean] :on_empty Defaults to :blank_row.
    #   When the collection is nil or empty, :blank_row (also :blank,
    #   :blank_detail, :empty, or +true+) keeps one row with blank detail
    #   cells; :omit (also :skip, :drop, :exclude, or +false+) drops the
    #   parent object entirely.
    #
    # @example Exporting orders with one row per line item
    #   OrderBlueprint.render_rows(
    #     orders,
    #     columns: [[:id], [:customer, :name], [:items, :sku], [:items, :price]],
    #     collection: :items
    #   )
    #   # => {
    #   #      columns: [:id, :name, :sku, :price],
    #   #      rows: [
    #   #        [1, 'Acme', 'A-1', 9.99],
    #   #        [1, 'Acme', 'A-2', 19.99],
    #   #        [2, 'Beta', 'B-1', 5.00]
    #   #      ]
    #   #    }
    #
    # @return [Hash{columns: Array<Symbol>, rows: Array<Array>}]
    # @raise [Blueprinter::Errors::InvalidRowPath] if a path crosses a second,
    #   undeclared collection or cannot be resolved against the Blueprint
    def render_rows(object, positional_columns = nil, positional_collection = nil,
                    columns: positional_columns, collection: positional_collection, **options)
      if columns.nil?
        raise ArgumentError,
              'render_rows requires ordered column paths, passed positionally or as `columns:`'
      end
      if collection.nil?
        raise ArgumentError,
              'render_rows requires exactly one collection to expand, passed positionally or as `collection:`'
      end

      on_empty = options.fetch(:on_empty, :blank_row)
      view_name = options[:view] || :default
      local_options = options.except(:view, :on_empty)

      renderer = RowRenderer.new(
        self,
        columns:,
        collection:,
        view_name:,
        local_options:,
        on_empty:
      )

      { columns: renderer.column_names, rows: renderer.render(object) }
    end

    # Converts an object into a Hash representation based on provided view.
    #
    # @param object [Object] the Object to convert into a Hash.
    # @param view_name [Symbol] the view
    # @param local_options [Hash] the options hash which requires a :view. Any
    #   additional key value pairs will be exposed during serialization.
    # @return [Hash]
    def hashify(object, view_name:, local_options:)
      raise Errors::UnknownView, "View '#{view_name}' is not defined" unless view_collection.view?(view_name)

      object = Blueprinter.configuration.extensions.pre_render(object, self, view_name, local_options)
      prepare_data(object, view_name, local_options)
    end

    # @deprecated This method is no longer supported, and was not originally intended to be public. This will be removed
    #   in the next minor release. If similar functionality is needed, use `.render_as_hash` instead.
    #
    # This is the magic method that converts complex objects into a simple hash
    # ready for JSON conversion.
    #
    # Note: we accept view (public interface) that is in reality a view_name,
    # so we rename it for clarity
    #
    # @api private
    def prepare(object, view_name:, local_options:)
      Blueprinter::Deprecation.report(
        <<~MESSAGE
          The `prepare` method is no longer supported will be removed in the next minor release.
          If similar functionality is needed, use `.render_as_hash` instead.
        MESSAGE
      )
      render_as_hash(object, view_name:, local_options:)
    end

    private

    attr_reader :blueprint, :options

    def prepare_data(object, view_name, local_options)
      # Since we're currently providing the current view in the local_options hash when we extract fields, we can merge
      # it in ahead of time to avoid allocating a new hash for every field extraction.
      local_options_with_view = local_options.merge(view: view_name)

      if array_like?(object)
        object.map do |obj|
          object_to_hash(
            obj,
            view_name:,
            local_options: local_options_with_view
          )
        end
      else
        object_to_hash(
          object,
          view_name:,
          local_options: local_options_with_view
        )
      end
    end

    def object_to_hash(object, view_name:, local_options:)
      result_hash = {}

      view_collection.fields_for(view_name).each do |field|
        next if field.skip?(field.name, object, local_options)

        value = field.extract(object, local_options)
        next if value.nil? && field.options[:exclude_if_nil]

        result_hash[field.name] = value
      end

      view_collection.transformers(view_name).each do |transformer|
        transformer.transform(result_hash, object, local_options)
      end

      result_hash
    end

    def jsonify(data)
      Blueprinter.configuration.jsonify(data)
    end

    def apply_root_key(object:, root:)
      return object unless root
      return { root => object } if root.is_a?(String) || root.is_a?(Symbol)

      raise(Errors::InvalidRoot)
    end

    def add_metadata(object:, metadata:, root:)
      return object unless metadata
      return object.merge(meta: metadata) if root

      raise(Errors::MetaRequiresRoot)
    end

    def build_result(object:, options:)
      view_name = options[:view] || :default

      prepared_object = hashify(
        object,
        view_name:,
        local_options: options.except(:view, :root, :meta)
      )
      object_with_root = apply_root_key(
        object: prepared_object,
        root: options[:root]
      )
      add_metadata(
        object: object_with_root,
        metadata: options[:meta],
        root: options[:root]
      )
    end
  end
end
