# frozen_string_literal: true

module Grape
  class Entity
    # Base error for polymorphic representation failures.
    class PolymorphicError < StandardError; end

    # A declarative type-to-presenter mapping used by polymorphic exposures.
    #
    # Rules map classes or modules to Grape::Entity subclasses (or their
    # constant names, which are constantized lazily). A special +:default+
    # key declares the fallback presenter.
    #
    # At representation time each object is dispatched individually:
    #
    # * +nil+ is never dispatched and always represents as +nil+;
    # * when one or more class rules match, the most specific one
    #   (nearest in the object's ancestor chain) wins;
    # * otherwise, when module rules match, the single most specific module
    #   wins - a module is more specific than another when it includes it.
    #   If no unique most specific module exists an
    #   {AmbiguousPolymorphicMatch} error is raised instead of guessing
    #   by registration order;
    # * otherwise the +:default+ presenter is used when declared;
    # * otherwise an {UnmappedPolymorphicType} error is raised.
    #
    # Mappings are deep-duped together with their exposure when an Entity
    # subclass is created, so extending a subclass mapping never pollutes
    # the parent entity or sibling representations.
    class PolymorphicMapping
      # Raised when several module rules match an object and no unique
      # most specific rule can be determined.
      class AmbiguousPolymorphicMatch < PolymorphicError; end

      # Raised when no rule and no default matches an object.
      class UnmappedPolymorphicType < PolymorphicError; end

      DEFAULT_KEY = :default

      # @return [Hash] rules mapping classes/modules to presenters
      attr_reader :rules

      def initialize(rules = {})
        @rules = {}
        @presenter_cache = {}
        @default = nil
        merge!(rules)
      end

      # Adds rules to the mapping. Accepts a hash of Class/Module =>
      # presenter pairs plus an optional +:default+ fallback entry.
      # Existing rules for the same type are replaced.
      #
      # @return [self]
      def merge!(rules)
        unless rules.is_a?(Hash)
          raise ArgumentError, "Polymorphic mapping rules must be a Hash, got #{rules.inspect}."
        end

        rules.each do |type, presenter|
          if default_key?(type)
            @default = presenter
            @presenter_cache.delete(DEFAULT_KEY)
          else
            add_rule(type, presenter)
          end
        end
        self
      end

      # @return [Object, nil] the fallback presenter, if declared
      def default
        @default
      end

      def default?
        !@default.nil?
      end

      # Resolves the presenter class for the given object.
      #
      # @param object [Object] the object to be represented
      # @return [Class] the Grape::Entity subclass to represent it with
      # @raise [AmbiguousPolymorphicMatch] when module rules are ambiguous
      # @raise [UnmappedPolymorphicType] when nothing matches and no default
      def resolve(object)
        matches = @rules.keys.select { |type| type === object }

        class_rule = most_specific_class(matches, object)
        return presenter_for(class_rule) if class_rule

        module_rule = most_specific_module(matches, object) if matches.any?
        return presenter_for(module_rule) if module_rule

        return default_presenter if default?

        raise UnmappedPolymorphicType,
              "No polymorphic presenter matches #{object.inspect} (#{object.class}) " \
              'and no :default presenter was declared.'
      end

      def ==(other)
        other.is_a?(PolymorphicMapping) &&
          rules == other.rules &&
          default == other.default
      end

      private

      def initialize_copy(other)
        super
        @rules = other.rules.dup
        @presenter_cache = {}
      end

      def add_rule(type, presenter)
        unless type.is_a?(Module)
          raise ArgumentError,
                "Polymorphic mapping keys must be classes or modules, got #{type.inspect}."
        end

        @rules[type] = presenter
        @presenter_cache.delete(type)
      end

      def default_key?(type)
        type.respond_to?(:to_sym) && type.to_sym == DEFAULT_KEY
      end

      # Class rules are totally ordered by inheritance, so the rule nearest
      # to the object's own class in its ancestor chain always wins.
      def most_specific_class(matches, object)
        classes = matches.select { |type| type.is_a?(Class) }
        return nil if classes.empty?

        ancestors = object.class.ancestors
        classes.min_by { |type| ancestors.index(type) }
      end

      # Module rules are only partially ordered (a module is more specific
      # than the modules it includes). When several incomparable modules
      # match there is no unique winner, so we refuse to guess.
      def most_specific_module(matches, object)
        undominated = matches.reject do |mod|
          matches.any? { |other| !other.equal?(mod) && other.ancestors.include?(mod) }
        end

        return undominated.first if undominated.size == 1

        raise AmbiguousPolymorphicMatch,
              "Ambiguous polymorphic presenters for #{object.inspect} (#{object.class}): " \
              "#{undominated.map(&:to_s).join(', ')} all match and none is more specific."
      end

      def presenter_for(type)
        @presenter_cache[type] ||= constantize(@rules[type])
      end

      def default_presenter
        @presenter_cache[DEFAULT_KEY] ||= constantize(@default)
      end

      def constantize(presenter)
        presenter.respond_to?(:constantize) ? presenter.constantize : presenter
      end
    end
  end
end
