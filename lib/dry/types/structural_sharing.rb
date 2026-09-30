# frozen_string_literal: true

module Dry
  module Types
    # Per-call context flag for the structural sharing coercion mode.
    #
    # When enabled (via the `:structural_sharing` option accepted by
    # {Schema#apply}, {Schema#call_unsafe} and {Schema#call_safe}), container
    # types ({Schema} and {Array::Member}) track whether coercion actually
    # changed anything — keys, element identity, order and presence — and
    # return the original container object instead of a rebuilt one when
    # nothing changed. Containers on paths where any transformation happened
    # are always rebuilt.
    #
    # @api private
    module StructuralSharing
      KEY = :__dry_types_structural_sharing__

      # Whether structural sharing is enabled for the current call
      #
      # @return [Boolean]
      #
      # @api private
      def self.enabled?
        ::Thread.current[KEY] || false
      end

      # Enable structural sharing for the duration of the block
      #
      # @return [Object] block result
      #
      # @api private
      def self.within
        previous = ::Thread.current[KEY]
        ::Thread.current[KEY] = true
        yield
      ensure
        ::Thread.current[KEY] = previous
      end
    end
  end
end
