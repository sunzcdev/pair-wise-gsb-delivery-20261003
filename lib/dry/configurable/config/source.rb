# frozen_string_literal: true

module Dry
  module Configurable
    class Config
      # Describes where the value of a single setting path originates.
      #
      # This intentionally carries no file name or call stack: it only records the category of
      # the source and whether the value was explicitly assigned.
      #
      # @api public
      class Source
        # The value still comes from the setting's default; it was never explicitly assigned
        # nor inherited.
        #
        # @return [Symbol]
        #
        # @api public
        DEFAULT = :default

        # The value was inherited from a superclass config and has not been assigned locally.
        #
        # @return [Symbol]
        #
        # @api public
        INHERITED = :inherited

        # The value was assigned on this config itself (via the writer, `#update`, or
        # `#configure`).
        #
        # @return [Symbol]
        #
        # @api public
        LOCAL = :local

        # @api private
        def self.[](origin, explicit)
          new(origin, explicit)
        end

        # @return [Symbol] one of {DEFAULT}, {INHERITED}, or {LOCAL}
        #
        # @api public
        attr_reader :origin

        # Whether the value was explicitly assigned on the config that owns it.
        #
        # Assigning a value equal to the default still counts as an explicit assignment, as does
        # assigning a value on a parent and inheriting it.
        #
        # @return [Boolean]
        #
        # @api public
        attr_reader :explicit

        # @api private
        def initialize(origin, explicit)
          @origin = origin
          @explicit = explicit
          freeze
        end

        # @return [Boolean]
        #
        # @api public
        def default?
          origin.equal?(DEFAULT)
        end

        # @return [Boolean]
        #
        # @api public
        def inherited?
          origin.equal?(INHERITED)
        end

        # @return [Boolean]
        #
        # @api public
        def local?
          origin.equal?(LOCAL)
        end

        # @return [Boolean]
        #
        # @api public
        alias_method :explicit?, :explicit

        # @api private
        def with(origin: self.origin, explicit: self.explicit)
          self.class.new(origin, explicit)
        end

        # @api private
        def ==(other)
          other.is_a?(self.class) &&
            origin.equal?(other.origin) &&
            explicit.equal?(other.explicit)
        end
        alias_method :eql?, :==

        # @api private
        def hash
          [self.class, origin, explicit].hash
        end

        # @return [String]
        #
        # @api public
        def inspect
          "#<#{self.class} origin=#{origin.inspect} explicit=#{explicit}>"
        end
      end
    end
  end
end
