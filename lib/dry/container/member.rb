# frozen_string_literal: true

require "dry/container/item/factory"

module Dry
  class Container
    # A named member of a {Collection}. Wraps a regular container item while
    # carrying stable metadata (name, tags, order, source) used to resolve and
    # order sets of handlers.
    #
    # @api public
    class Member
      # @return [String] stable name of the member within its collection
      attr_reader :name

      # @return [Array<String>] tags the member is addressable by
      attr_reader :tags

      # @return [Integer, nil] explicit ordering weight
      attr_reader :order

      # @return [String] where the member was registered from
      attr_reader :source

      # @return [Integer] zero-based registration index within its collection
      #
      # @api private
      attr_accessor :sequence

      # @return [Dry::Container::Item::Callable, Dry::Container::Item::Memoizable]
      attr_reader :item

      # @param [Mixed] name
      #   Stable name of the member inside its collection
      # @param [Mixed] item
      #   The item (value, proc or block) to register
      # @param [Hash] options
      # @option options [Symbol, Array] :tags
      #   Additional tags the member answers to
      # @option options [Integer] :order
      #   Explicit weight; members are sorted by it (then registration order)
      # @option options [String] :source
      #   Origin of the binding, reported on name conflicts
      # @option options [String] :collection_tag
      #   Tag of the collection the member belongs to
      #
      # @api public
      def initialize(name, item, options = {})
        @name = name.to_s.dup.freeze
        @order = options[:order]
        @source = (options[:source] || DEFAULT_SOURCE).to_s
        @item = factory.call(item, options)
        tags = [
          *(options[:collection_tag] && options[:collection_tag].to_s),
          @name,
          *Array(options[:tags]).map(&:to_s)
        ]
        @tags = tags.freeze
      end

      # Resolve the member, honouring its own callable/memoize settings
      #
      # @return [Mixed]
      #
      # @api public
      def call
        item.call
      end

      # @param [String] tag
      #
      # @return [Bool]
      #
      # @api public
      def tagged?(tag)
        tags.include?(tag.to_s)
      end

      # Build a new member with its resolved value transformed
      #
      # @return [Dry::Container::Member]
      #
      # @api private
      def map(func)
        @item = item.map(func)
        self
      end

      # @api private
      def factory
        @factory ||= ::Dry::Container::Item::Factory.new
      end

      private

      DEFAULT_SOURCE = "container"
      private_constant :DEFAULT_SOURCE
    end
  end
end
