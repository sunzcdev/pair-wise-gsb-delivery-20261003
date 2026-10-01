# frozen_string_literal: true

module Grape
  class Entity
    module Exposure
      # Declarative, order-independent mapping from runtime object types
      # to the Entity that should represent them.
      #
      # Keys are classes or modules matched with +===+ (so a class matches
      # instances and a module matches objects including it).  Among all
      # matching keys the most specific one wins.  Two class keys are never
      # ambiguous under single inheritance, while two unrelated module keys
      # matching the same object raise {AmbiguousPolymorphicMatch} instead of
      # relying on registration order.
      #
      # The special key +:default+ provides an explicit fallback for objects
      # whose type matches no rule.
      class PolymorphicMapping
        # Reserved key selecting the explicit fallback presenter.
        DEFAULT_KEY = :default

        # Raised when more than one equally-specific rule matches an object.
        class AmbiguousPolymorphicMatch < StandardError; end

        # Raised when an object matches no rule and no fallback was declared.
        class UnmappedPolymorphicType < StandardError; end

        class << self
          # Coerces a mapping declaration into a PolymorphicMapping.
          #
          # @param mapping [PolymorphicMapping, Hash] an existing mapping is
          #   returned as-is, a Hash of type => presenter is compiled.
          def coerce(mapping)
            mapping.is_a?(self) ? mapping : new(mapping)
          end
        end

        # @param rules [Hash] map of Class/Module (or +:default+) to a
        #   Grape::Entity subclass or a constantizable String.
        def initialize(rules = {})
          raise ArgumentError, 'polymorphic mapping rules must be a Hash' unless rules.is_a?(Hash)
          raise ArgumentError, 'polymorphic mapping cannot be empty' if rules.empty?

          rules.each_pair { |key, presenter| validate_rule!(key, presenter) }

          @rules = rules.dup.freeze
          freeze
        end

        # @return [Hash] the raw, frozen rule map.
        def rules
          @rules
        end

        # Merges additional rules and returns a *new* mapping; the receiver
        # is never mutated, so extending a mapping in a subclass cannot
        # pollute the parent class or any already-built representation config.
        #
        # @param extra_rules [Hash]
        # @return [PolymorphicMapping]
        def merge(extra_rules = {})
          self.class.new(@rules.merge(extra_rules))
        end

        # Resolves the presenter configured for +object+.
        #
        # @param object [Object] the nested object to represent
        # @return [Class, nil] the matching Entity class, or nil for a nil
        #   object.
        # @raise [AmbiguousPolymorphicMatch] when multiple equally-specific
        #   module rules match
        # @raise [UnmappedPolymorphicType] when nothing matches and no
        #   fallback was declared
        def entity_for(object)
          return nil if object.nil?

          key = key_for(object)
          return nil if key.nil?

          resolve_presenter(@rules[key])
        end

        # Represents a single object or a collection of objects, dispatching
        # every element individually according to its actual type.
        #
        # @param object [Object, Array, nil]
        # @param options [Hash, Grape::Entity::Options] options forwarded to
        #   each selected Entity.
        # @return [Object, Array, nil]
        def represent(object, options = {})
          if object.respond_to?(:to_ary)
            object.to_ary.map { |item| represent_one(item, options) }
          else
            represent_one(object, options)
          end
        end

        def ==(other)
          other.is_a?(self.class) && @rules == other.rules
        end

        private

        def represent_one(object, options)
          presenter = entity_for(object)
          return nil if object.nil?
          return object if presenter.nil?

          presenter.represent(object, options)
        end

        def key_for(object)
          matches = @rules.keys.reject { |key| key == DEFAULT_KEY }.select { |key| key === object }

          if matches.empty?
            @rules.key?(DEFAULT_KEY) ? DEFAULT_KEY : unmapped!(object)
          else
            most_specific(matches, object)
          end
        end

        # Picks the unique most specific key.  Class keys always outrank
        # module keys for a concrete instance.  Two classes are totally
        # ordered by inheritance; two modules are ordered by inclusion and
        # reported as ambiguous when they are incomparable.
        def most_specific(keys, object)
          class_keys, module_keys = keys.partition { |key| key.is_a?(Class) }

          return disambiguate_modules(module_keys, object) if class_keys.empty?

          class_keys.min_by { |klass| object.class.ancestors.index(klass) }
        end

        def disambiguate_modules(modules, object)
          modules.min do |a, b|
            order = a <=> b
            next order unless order.nil?

            # Incomparable modules (neither includes the other) both match and
            # there is no unique most specific rule.
            raise ambiguous!(a, b, object)
          end
        end

        def ambiguous!(key_a, key_b, object)
          AmbiguousPolymorphicMatch.new(
            "ambiguous polymorphic mapping for #{object.class}: " \
            "#{key_a} and #{key_b} both match with no unique most specific rule"
          )
        end

        def unmapped!(object)
          raise UnmappedPolymorphicType,
                "no polymorphic presenter registered for #{object.class} " \
                "(and no :default fallback)"
        end

        def resolve_presenter(presenter)
          return presenter unless presenter.respond_to?(:constantize)

          presenter.constantize
        end

        def validate_rule!(key, presenter)
          valid_key = key == DEFAULT_KEY || key.is_a?(Module)
          raise ArgumentError, "invalid polymorphic mapping key #{key.inspect}" unless valid_key

          valid_presenter = presenter.is_a?(String) ||
                            (presenter.is_a?(Class) && presenter <= Grape::Entity)
          return if valid_presenter

          raise ArgumentError, "invalid polymorphic presenter #{presenter.inspect} for key #{key.inspect}"
        end
      end
    end
  end
end
