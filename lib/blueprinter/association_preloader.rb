# frozen_string_literal: true

require 'blueprinter/errors/preload_result'

module Blueprinter
  # @api private
  #
  # Runs an association's +preload+ callable once for the unique set of
  # objects in the current render scope that actually need the association,
  # then exposes the batch-fetched values by object identity.
  class AssociationPreloader
    # Local-options key under which the per-association identity maps are
    # carried through a single hashify call.
    CONTEXT_KEY = :__blueprinter_preload_context__

    # Builds a preload context for one render scope.
    #
    # Associations are selected purely from the supplied fields (i.e. the
    # resolved view, after excludes), and an object is passed to a preloader
    # only when that association's own +if+/+unless+ conditions pass for it.
    #
    # @param fields [Array<Blueprinter::Field>] resolved fields for the view
    # @param objects [Array<Object>] objects in this render scope, in output order
    # @param local_options [Hash]
    # @return [Hash] preload context keyed by association name. Aliased
    #   associations (same method, different +:name+) each get their own
    #   preloaded map and their own preloader invocation.
    def self.preload!(fields, objects, local_options)
      context = {}

      fields.each do |field|
        preloader = field.options[:preload]
        next unless preloader

        needed = unique_objects_needing(field, objects, local_options)
        next if needed.empty?

        context[field.name] = coerce_result(preloader.call(needed), field, needed)
      end

      context
    end

    # Fetches a preloaded association value for an object.
    #
    # @return [Object] the preloaded value (+nil+ is a valid, explicitly
    #   preloaded empty association)
    # @raise [Blueprinter::Errors::PreloadResult] if no value was preloaded
    #   for the object; rendering never silently falls back to per-object
    #   extraction
    def self.fetch(context, field, object)
      map = context && context[field.name]

      unless map && map.key?(object)
        raise(
          Errors::PreloadResult,
          "preload for association '#{field.name}' returned no result for #{object.inspect}"
        )
      end

      map[object]
    end

    def self.unique_objects_needing(field, objects, local_options)
      seen = {}.compare_by_identity
      needed = []

      objects.each do |object|
        next if seen.key?(object)
        next if field.skip?(field.name, object, local_options)

        seen[object] = true
        needed << object
      end

      needed
    end

    private_class_method def self.coerce_result(result, field, needed)
      unless result.is_a?(Hash)
        raise(
          Errors::PreloadResult,
          "preload for association '#{field.name}' must return a Hash keyed by object, " \
          "got #{result.class}"
        )
      end

      map = {}.compare_by_identity

      needed.each do |object|
        unless result.key?(object)
          raise(
            Errors::PreloadResult,
            "preload for association '#{field.name}' is missing a result for #{object.inspect}"
          )
        end

        map[object] = result[object]
      end

      map
    end
  end
end
