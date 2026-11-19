# encoding: binary
# frozen_string_literal: true

module AMQ
  module Protocol
    # Raised when strict parameter mode rejects a value that does not match
    # the type declared for an AMQP method argument in the protocol spec.
    class InvalidParameterError < Protocol::Error
      def initialize(method_name, argument_name, expected, actual)
        super("#{method_name} argument #{argument_name} has to be #{expected}, got #{actual.inspect} (#{actual.class})")
      end
    end

    # Hand-written counterpart of the generated method classes. It carries the
    # strict parameter mode switch and the value checks shared by every
    # generated AMQP method. Generated code only describes each method's
    # arguments through a PARAMETERS constant and calls into these helpers.
    module StrictParameters
      RANGES = {
        :octet    => 0..255,
        :short    => 0..65_535,
        :long     => 0..4_294_967_295,
        :longlong => 0..18_446_744_073_709_551_615
      }.freeze

      class << self
        def enabled?
          @enabled == true
        end

        def enable!
          @enabled = true
        end

        def disable!
          @enabled = false
        end

        def validate!(method_name, parameters, arguments)
          return unless enabled?

          parameters.each do |(name, type, optional)|
            value = arguments.fetch(name)
            next if value.nil? && optional
            check_value!(method_name, name, type, value)
          end
        end

        private

        def check_value!(method_name, name, type, value)
          case type
          when :bit
            unless value.equal?(true) || value.equal?(false) || value.nil?
              raise InvalidParameterError.new(method_name, name, "true, false or nil", value)
            end
          when :octet, :short, :long, :longlong
            range = RANGES.fetch(type)
            unless value.is_a?(Integer) && range.include?(value)
              raise InvalidParameterError.new(method_name, name, "an Integer in #{range}", value)
            end
          when :shortstr
            unless value.is_a?(String) && value.bytesize <= 255
              raise InvalidParameterError.new(method_name, name, "a String of at most 255 bytes", value)
            end
          when :longstr
            unless value.is_a?(String)
              raise InvalidParameterError.new(method_name, name, "a String", value)
            end
          when :table
            unless value.is_a?(Hash)
              raise InvalidParameterError.new(method_name, name, "a Hash", value)
            end
          when :timestamp
            unless value.is_a?(Time) || value.is_a?(Integer)
              raise InvalidParameterError.new(method_name, name, "a Time or Integer", value)
            end
          else
            raise InvalidParameterError.new(method_name, name, type.to_s, value)
          end
        end
      end
    end
  end
end
