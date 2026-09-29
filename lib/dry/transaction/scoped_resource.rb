# frozen_string_literal: true

module Dry
  module Transaction
    # A resource acquired by a +scope+ step, pairing the handle handed to the
    # remaining steps with the action that releases it when the scope exits.
    #
    # Scope step operations return a ScopedResource (or a bare handle, which
    # is released via +close+). The release action can be any callable, or a
    # method name on the handle:
    #
    #   ScopedResource[connection]                    # releases via connection.close
    #   ScopedResource[connection, :disconnect]       # releases via connection.disconnect
    #   ScopedResource[connection, -> { conn.reset }] # releases via the callable
    #
    # @api public
    class ScopedResource
      # @api private
      DEFAULT_RELEASE = :close

      # Coerces an operation's return value into a ScopedResource.
      #
      # @api private
      def self.[](resource, release = DEFAULT_RELEASE)
        resource.is_a?(self) ? resource : new(resource, release)
      end

      # The acquired resource, passed as input to the remaining steps.
      attr_reader :handle

      # Zero-argument callable releasing the resource.
      attr_reader :release

      def initialize(handle, release = DEFAULT_RELEASE)
        @handle = handle
        @release =
          case release
          when Symbol then -> { handle.public_send(release) }
          else release
          end

        unless @release.respond_to?(:call)
          raise ArgumentError, "release must be a callable or a method name on the handle"
        end
      end
    end
  end
end
