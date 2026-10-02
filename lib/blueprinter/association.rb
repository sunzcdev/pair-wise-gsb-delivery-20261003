# frozen_string_literal: true

require 'blueprinter/field'
require 'blueprinter/blueprint_validator'
require 'blueprinter/errors/missing_preload_result'
require 'blueprinter/extractors/association_extractor'

module Blueprinter
  # @api private
  class Association < Field
    # Key under which preloaded association values are kept in local_options
    # while rendering. Maps an Association field to the Hash returned by its
    # :preload callable (itself keyed by object).
    PRELOAD_STORE_KEY = :__blueprinter_preloaded_associations__

    # @param method [Symbol] The method to call on the source object to retrieve the associated data
    # @param name [Symbol] The name of the association as it will appear when rendered
    # @param blueprint [Blueprinter::Base] The blueprint to use for rendering the association
    # @param view [Symbol] The view to use in conjunction with the blueprint
    # @param parent_blueprint [Blueprinter::Base] The blueprint that this association is being defined within
    # @param extractor [Blueprinter::Extractor] The extractor to use when retrieving the associated data
    # @param options [Hash] Pass :preload with a callable to bulk-load this association once per render.
    #   The callable receives the objects that will render the association and must return a Hash
    #   keyed by those objects (nil values represent empty associations).
    #
    # @return [Blueprinter::Association]
    def initialize(method:, name:, blueprint:, view:, parent_blueprint:, extractor: AssociationExtractor.new, options: {})
      BlueprintValidator.validate!(blueprint)
      validate_preload!(extractor, options[:preload]) if options[:preload]

      super(
        method,
        name,
        extractor,
        parent_blueprint,
        options.merge(
          blueprint:,
          view:,
          association: true
        )
      )
    end

    def extract(object, local_options)
      return super unless options[:preload]

      extractor.extract_preloaded(preloaded_value(object, local_options), object, local_options, options)
    end

    private

    def validate_preload!(extractor, preload)
      raise ArgumentError, ':preload must be a callable' unless preload.respond_to?(:call)

      return if extractor.is_a?(AssociationExtractor)

      raise ArgumentError, ':preload is only supported with the default AssociationExtractor'
    end

    def preloaded_value(object, local_options)
      results = local_options.fetch(PRELOAD_STORE_KEY, {})[self]

      if !results.respond_to?(:key?) || !results.key?(object)
        raise Errors::MissingPreloadResult, name
      end

      results[object]
    end
  end
end
