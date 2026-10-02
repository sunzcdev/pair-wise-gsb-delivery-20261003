# frozen_string_literal: true

require 'blueprinter/extractor'
require 'blueprinter/empty_types'
require 'blueprinter/field_projection'

module Blueprinter
  # @api private
  class AssociationExtractor < Extractor
    include EmptyTypes

    def initialize
      @extractor = Blueprinter.configuration.default_extractor
    end

    def extract(association_name, object, local_options, options = {})
      projection = options[FieldProjection::OPTION_KEY]
      projection_path = options[FieldProjection::PATH_OPTION_KEY]

      extraction_options = options.except(
        :default,
        :default_if,
        FieldProjection::OPTION_KEY,
        FieldProjection::PATH_OPTION_KEY
      )

      value = @extractor.extract(association_name, object, local_options, extraction_options)
      return default_value(options) if use_default_value?(value, options[:default_if])

      # Merge in association options - supports both static Hash and dynamic Proc
      local_options = merge_association_options(local_options, options[:options], object)

      view = options[:view] || :default
      blueprint = association_blueprint(options[:blueprint], value)
      hashify_arguments = { view_name: view, local_options:, projection: }
      hashify_arguments[:projection_path] = projection_path if projection
      blueprint.hashify(value, **hashify_arguments)
    end

    private

    def merge_association_options(local_options, association_options, object)
      return local_options unless association_options

      additional_options = if association_options.respond_to?(:call)
                             association_options.call(object)
                           else
                             association_options
                           end

      local_options.merge(additional_options)
    end

    def default_value(association_options)
      return association_options.fetch(:default) if association_options.key?(:default)

      Blueprinter.configuration.association_default
    end

    def association_blueprint(blueprint, value)
      blueprint.is_a?(Proc) ? blueprint.call(value) : blueprint
    end
  end
end
