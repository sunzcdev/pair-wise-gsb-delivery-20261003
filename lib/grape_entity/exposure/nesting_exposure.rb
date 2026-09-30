# frozen_string_literal: true

module Grape
  class Entity
    module Exposure
      class NestingExposure < Base
        attr_reader :nested_exposures

        def setup(nested_exposures = [])
          @nested_exposures = NestedExposures.new(nested_exposures)
        end

        def dup_args
          [*super, @nested_exposures.map(&:dup)]
        end

        def ==(other)
          super && @nested_exposures == other.nested_exposures
        end

        def nesting?
          true
        end

        def find_nested_exposure(attribute)
          nested_exposures.find_by(attribute)
        end

        def valid?(entity)
          nested_exposures.all? { |e| e.valid?(entity) }
        end

        def value(entity, options)
          map_entity_exposures(entity, options) do |exposure, nested_options|
            exposure.value(entity, nested_options)
          end
        end

        def serializable_value(entity, options)
          map_entity_exposures(entity, options) do |exposure, nested_options|
            exposure.serializable_value(entity, nested_options)
          end
        end

        def valid_value_for(key, entity, options)
          new_options = nesting_options_for(options)

          key_exposures = normalized_exposures(entity, new_options).select { |e| e.key(entity) == key }

          key_exposures.map do |exposure|
            exposure.with_attr_path(entity, new_options) do
              exposure.valid_value(entity, new_options)
            end
          end.last
        end

        # if we have any nesting exposures with the same name.
        # delegate :deep_complex_nesting?(entity), to: :nested_exposures
        def deep_complex_nesting?(entity)
          nested_exposures.deep_complex_nesting?(entity)
        end

        private

        def nesting_options_for(options)
          if @key
            options.for_nesting(@key)
          else
            options
          end
        end

        def easy_normalized_exposures(entity, options)
          nested_exposures.select do |exposure|
            exposure.with_attr_path(entity, options) do
              exposure.should_expose?(entity, options)
            end
          end
        end

        # This method 'merges' subsequent nesting exposures with the same name if it's needed
        def normalized_exposures(entity, options)
          return easy_normalized_exposures(entity, options) unless deep_complex_nesting?(entity) # optimization

          table = nested_exposures.each_with_object({}) do |exposure, output|
            should_expose = exposure.with_attr_path(entity, options) do
              exposure.should_expose?(entity, options)
            end
            next unless should_expose

            output[exposure.key(entity)] ||= []
            output[exposure.key(entity)] << exposure
          end

          table.map do |key, exposures|
            last_exposure = exposures.last

            if last_exposure.nesting?
              # For the given key if the last candidates for exposing are nesting then combine them.
              nesting_tail = []
              exposures.reverse_each do |exposure|
                nesting_tail.unshift exposure if exposure.nesting?
              end
              new_nested_exposures = nesting_tail.flat_map(&:nested_exposures)
              NestingExposure.new(key, {}, [], new_nested_exposures).tap do |new_exposure|
                if nesting_tail.any? { |exposure| exposure.deep_complex_nesting?(entity) }
                  new_exposure.instance_variable_set(:@deep_complex_nesting, true)
                end
              end
            else
              last_exposure
            end
          end
        end

      def map_entity_exposures(entity, options)
        new_options = nesting_options_for(options)

        unless new_options[:partial_errors]
          return map_full_entity_exposures(entity, new_options) { |*args| yield(*args) }
        end

        map_partial_entity_exposures(entity, new_options) { |*args| yield(*args) }
      end

        def map_full_entity_exposures(entity, options)
          output = OutputBuilder.new(entity)

          normalized_exposures(entity, options).each_with_object(output) do |exposure, out|
            exposure.with_attr_path(entity, options) do
              result = yield(exposure, options)
              out.add(exposure, result)
            end
          end
        end

        def map_partial_entity_exposures(entity, options)
          output = OutputBuilder.new(entity)

          nested_exposures.each_with_object(output) do |exposure, out|
            process_partial_exposure(exposure, entity, options, out) { yield(exposure, options) }
          end
        end

        def process_partial_exposure(exposure, entity, options, output)
          result = process_partial_exposure_at_path(exposure, entity, options) { yield }

          return if result == :omitted || result == :error
          return output.add(exposure, nil, result[1]) if result.first == :failed

          output.add(exposure, result.first, result.last)
        end

        def process_partial_exposure_at_path(exposure, entity, options)
          # The external path part is computed up front and pushed onto
          # the stack outside of the value rescue: a failure here (e.g.
          # a raising +:as+ or +:attr_path+ proc) surfaces as the
          # original exception rather than being swallowed into an empty
          # value or re-executing the failing proc.
          path_part = exposure.attr_path(entity, options)

          options.with_attr_path(path_part) do
            should_expose = partial_should_expose?(exposure, entity, options)
            next :error if should_expose == false || should_expose == :error

            begin
              value = yield
              external_key = exposure.output_key(entity)
            rescue StandardError => e
              strategy = exposure.record_partial_error(entity, options, e)
              next :omitted if strategy == :omit

              # When the key is a proc the path part above is the raw
              # proc; resolve it for the output without re-running the
              # value getter a second time.
              external_key = path_part.respond_to?(:call) ? exposure.output_key(entity) : path_part
              next [:failed, external_key]
            end

            [value, external_key]
          end
        end

        def partial_should_expose?(exposure, entity, options)
          exposure.should_expose?(entity, options)
        rescue StandardError => e
          exposure.record_partial_error(entity, options, e)
          :error
        end
      end
    end
  end
end

require 'grape_entity/exposure/nesting_exposure/nested_exposures'
require 'grape_entity/exposure/nesting_exposure/output_builder'
