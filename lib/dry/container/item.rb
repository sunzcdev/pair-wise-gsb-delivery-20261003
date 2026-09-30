# frozen_string_literal: true

require "dry/container/error"

module Dry
  class Container
    # Base class to abstract Memoizable and Callable implementations
    #
    # @api abstract
    #
    class Item
      # @return [Mixed] the item to be solved later
      attr_reader :item

      # @return [Hash] the options to memoize, call or no.
      attr_reader :options

      # @return [Array<Array(String, String)>] pairs of [argument name, key]
      #   that are resolved eagerly and injected into the factory
      attr_reader :dependencies

      # @return [Array<Array(String, String)>] pairs of [argument name, key]
      #   that are resolved lazily through callable dependency values
      attr_reader :lazy_dependencies

      # @api abstract
      def initialize(item, options = {})
        @item = item
        @dependencies = self.class.normalize_dependencies(options[:depends_on])
        @lazy_dependencies =
          self.class.normalize_dependencies(options[:lazy_depends_on])

        unless @dependencies.empty? && @lazy_dependencies.empty?
          if options.key?(:call) && options[:call] == false
            raise Error, "Items declaring dependencies cannot be registered with call: false"
          end

          unless item.respond_to?(:call)
            raise Error, "Only callable items can declare dependencies"
          end
        end

        @options = {
          call:
            item.is_a?(::Proc) && item.parameters.empty? ||
              !@dependencies.empty? ||
              !@lazy_dependencies.empty?
        }.merge(options)

        ensure_unique_dependencies!
      end

      # Normalize a dependency declaration into [name, key] pairs
      #
      # @return [Array<Array(String, String)>]
      #
      # @private
      def self.normalize_dependencies(declaration)
        case declaration
        when nil
          []
        when ::Array
          declaration.map { |entry| normalize_entry(entry) }
        when ::Hash
          declaration.map { |name, key| [name.to_s, key.to_s] }
        else
          raise Error, "Dependencies must be given as an Array or a Hash"
        end
      end

      # @private
      def self.normalize_entry(entry)
        case entry
        when ::Array
          raise Error, "Dependency entries need a name and a key" unless entry.size == 2

          [entry[0].to_s, entry[1].to_s]
        else
          name = entry.to_s
          [name, name]
        end
      end

      # @api abstract
      def call(**_dependencies)
        raise NotImplementedError
      end

      # @private
      def value?
        !callable?
      end

      # @private
      def callable?
        options[:call]
      end

      # Build a new item with transformation applied
      #
      # @private
      def map(func)
        if callable?
          self.class.new(
            -> **dependencies { func.(item.call(**dependencies)) },
            dependency_options
          )
        else
          self.class.new(func.(item), options)
        end
      end

      private

      # @private
      def dependency_options
        options.merge(
          depends_on: dependency_declaration(@dependencies),
          lazy_depends_on: dependency_declaration(@lazy_dependencies)
        )
      end

      # @private
      def dependency_declaration(pairs)
        pairs.each_with_object({}) { |(name, key), declaration|
          declaration[name.to_sym] = key
        }
      end

      # @private
      def ensure_unique_dependencies!
        names = (@dependencies + @lazy_dependencies).map(&:first)
        duplicate = names.detect { |name| names.count(name) > 1 }

        if duplicate
          raise Error, "Dependency #{duplicate.inspect} is declared more than once"
        end
      end
    end
  end
end
