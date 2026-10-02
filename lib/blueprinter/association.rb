# frozen_string_literal: true

require 'blueprinter/field'
require 'blueprinter/blueprint_validator'
require 'blueprinter/association_preloader'
require 'blueprinter/extractors/association_extractor'

module Blueprinter
  # @api private
  class Association < Field
    # @param method [Symbol] The method to call on the source object to retrieve the associated data
    # @param name [Symbol] The name of the association as it will appear when rendered
    # @param blueprint [Blueprinter::Base] The blueprint to use for rendering the association
    # @param view [Symbol] The view to use in conjunction with the blueprint
    # @param parent_blueprint [Blueprinter::Base] The blueprint that this association is being defined within
    # @param extractor [Blueprinter::Extractor] The extractor to use when retrieving the associated data
    # @param options [Hash]
    # @option options [#call] :preload An optional callable that receives the
    #   unique objects in the current render scope which actually output this
    #   association (after view, exclusion and +:if+/+:unless+ filtering) and
    #   returns a Hash keyed by those objects. The value for each object
    #   (including +nil+, meaning an explicitly empty association) is then used
    #   instead of extracting the association per object.
    #
    # @return [Blueprinter::Association]
    def initialize(method:, name:, blueprint:, view:, parent_blueprint:, extractor: AssociationExtractor.new, options: {})
      BlueprintValidator.validate!(blueprint)
      validate_preload!(options[:preload])

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

    private

    def validate_preload!(preload)
      return if preload.nil?
      return if preload.respond_to?(:call)

      raise ArgumentError, "preload for association '#{name}' must respond to #call"
    end
  end
end
