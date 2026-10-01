# frozen_string_literal: true

module Grape
  class Entity
    module Exposure
      # An exposure whose nested presenter is chosen at runtime from a
      # {PolymorphicMapping} based on the actual type of each represented
      # object (and, independently, every element of a collection).
      class PolymorphicRepresentExposure < Base
        attr_reader :mapping, :subexposure

        def setup(mapping, subexposure)
          @mapping = mapping
          @subexposure = subexposure
        end

        # Returns a copy of this exposure whose mapping is extended with
        # +extra_rules+.  Neither this exposure nor its mapping is mutated,
        # which keeps subclass rules away from the parent class and from any
        # representation configuration already built from it.
        def with_extra_rules(extra_rules)
          dup.tap do |copy|
            copy.instance_variable_set(:@mapping, @mapping.merge(extra_rules))
          end
        end

        def dup_args
          [*super, @mapping, @subexposure]
        end

        def ==(other)
          super &&
            @mapping == other.mapping &&
            @subexposure == other.subexposure
        end

        def value(entity, options)
          new_options = options.for_nesting(key(entity))
          mapping.represent(@subexposure.value(entity, options), new_options)
        end

        def serializable_value(entity, options)
          partial_output = valid_value(entity, options)
          serialize(partial_output)
        end

        def valid?(entity)
          @subexposure.valid? entity
        end

        def polymorphic?
          true
        end

        private

        # Collections are dispatched element-by-element, so serialize each
        # element individually (nil elements included) instead of relying on
        # the all-respond-to? array branch in Base.
        def serialize(output)
          if output.is_a?(Array)
            output.map { |item| serialize(item) }
          elsif output.respond_to?(:serializable_hash)
            output.serializable_hash
          elsif output.is_a?(Hash)
            output.each { |key, value| output[key] = value.serializable_hash if value.respond_to?(:serializable_hash) }
          else
            output
          end
        end
      end
    end
  end
end
