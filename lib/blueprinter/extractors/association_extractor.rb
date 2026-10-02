# frozen_string_literal: true

require 'blueprinter/extractor'
require 'blueprinter/association_preloader'
require 'blueprinter/empty_types'

module Blueprinter
  # @api private
  class AssociationExtractor < Extractor
    include EmptyTypes

    def initialize
      @extractor = Blueprinter.configuration.default_extractor
    end

    def extract(association_name, object, local_options, options = {})
      options_without_default = if options.key?(:default) || options.key?(:default_if)
                                  options.except(:default, :default_if)
                                else
                                  options
                                end

      field = options[:__field__]
      nested_local_options = strip_preload_context(local_options)

      value = if field&.options&.key?(:preload)
                AssociationPreloader.fetch(
                  local_options[AssociationPreloader::CONTEXT_KEY],
                  field,
                  object
                )
              else
                @extractor.extract(association_name, object, nested_local_options, options_without_default)
              end

      return default_value(options) if use_default_value?(value, options[:default_if])

      # Merge in association options - supports both static Hash and dynamic Proc
      local_options = merge_association_options(nested_local_options, options[:options], object)

      view = options[:view] || :default
      blueprint = association_blueprint(options[:blueprint], value)
      blueprint.hashify(value, view_name: view, local_options:)
    end

    private

    def strip_preload_context(local_options)
      return local_options unless local_options.key?(AssociationPreloader::CONTEXT_KEY)

      local_options.except(AssociationPreloader::CONTEXT_KEY)
    end

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
