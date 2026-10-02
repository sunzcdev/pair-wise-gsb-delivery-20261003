# frozen_string_literal: true

require 'blueprinter/errors/invalid_root'
require 'blueprinter/errors/meta_requires_root'
require 'blueprinter/errors/unknown_view'
require 'blueprinter/errors/streaming_root_unsupported'
require 'blueprinter/errors/streaming_meta_unsupported'
require 'blueprinter/deprecation'
require 'blueprinter/streamed_json_body'

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

    # Builds a chunked JSON array response body from a single-pass Enumerable
    # data source. Unlike {#render}, the source is not read until the returned
    # body is consumed, and each advance serializes only the next element.
    #
    # Fields, views, associations and transformers are rendered with the same
    # rules as {#render}. This entry only emits a top-level JSON array, so
    # :root and :meta are rejected before the source is ever read.
    #
    # @param enumerable [Enumerable] the data source, traversable once
    # @param options [Hash] the same options as {#render}, except :root/:meta
    # @option options [Symbol] :view Defaults to :default.
    # @return [Blueprinter::StreamedJsonBody] consumable, chunked response body
    # @raise [Blueprinter::Errors::StreamingRootUnsupported] if :root is given
    # @raise [Blueprinter::Errors::StreamingMetaUnsupported] if :meta is given
    #
    # @example Streaming to a socket
    #   body = PostBlueprint.render_as_stream(posts, view: :extended)
    #   body.each { |chunk| socket.write(chunk) }
    #
    # @example As a Rack response body
    #   [200, { 'Content-Type' => 'application/json' },
    #    PostBlueprint.render_as_stream(posts)]
    def render_as_stream(enumerable, options = {})
      StreamedJsonBody.new(enumerable, self, options)
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

    # @api private
    #
    # Runs render extensions once for a streamed collection, before any
    # element is pulled from it.
    def pre_render_collection(object, view_name, options)
      local_options = options.except(:view, :root, :meta)
      Blueprinter.configuration.extensions.pre_render(object, self, view_name, local_options)
    end

    # @api private
    #
    # Renders a single element of a streamed collection with the standard
    # field, association and transformer rules.
    def streamed_element_hash(object, options)
      view_name = options[:view] || :default
      local_options = options.merge(view: view_name)
      object_to_hash(object, view_name:, local_options:)
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
