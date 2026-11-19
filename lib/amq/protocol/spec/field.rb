# frozen_string_literal: true

module AMQ
  module Protocol
    module Spec
      # A single method argument or content property of a protocol version.
      #
      # A field's position in its owner is part of the wire format, so field
      # order is never rearranged. `type` is the resolved primitive type,
      # `domain`/`type_name` keep the declaration exactly as the description
      # source wrote it so descriptions can be serialized back losslessly.
      class Field
        NO_DEFAULT = Object.new.freeze
        # Sentinel for "the description declares no default for this field".
        UNDEFINED = NO_DEFAULT

        PRIMITIVE_TYPES = %w[
          bit
          octet
          short
          long
          longlong
          shortstr
          longstr
          table
          timestamp
        ].freeze

        attr_reader :name, :domain, :type_name, :type

        def initialize(name:, domain: nil, type: nil, type_name: nil, resolved_type: nil, default: NO_DEFAULT)          unless name.is_a?(String) && !name.empty?
            raise ArgumentError, "field name must be a non-empty String, got #{name.inspect}"
          end

          @name = name
          @domain = domain
          @type_name = type_name || type
          @type = resolved_type ||
                   (domain.nil? && Field::PRIMITIVE_TYPES.include?(@type_name) ? @type_name : nil)
          @default = default
        end

        # Declared default value, or nil when the description declares none.
        def default
          @default.equal?(NO_DEFAULT) ? nil : @default
        end

        # Whether the description declares a default for this field.
        def default?
          !@default.equal?(NO_DEFAULT)
        end

        # Name usable as a Ruby identifier (and as a hash key), e.g. :routing_key.
        # The protocol name itself is kept in #name.
        def ruby_name
          @ruby_name ||= name.tr("- ", "_").to_sym
        end

        def bit?
          type == "bit"
        end

        def resolved?
          !type.nil?
        end

        def declaration
          domain ? ["domain", domain] : ["type", type_name]
        end

        def with_type(resolved_type)
          self.class.new(
            name: name,
            domain: domain,
            type_name: domain ? nil : type_name,
            resolved_type: resolved_type,
            default: @default
          )
        end

        def to_h
          kind, value = declaration
          result = { "name" => name, kind => value }
          result["default-value"] = default if default?
          result
        end

        def ==(other)
          other.is_a?(Field) &&
            name == other.name &&
            domain == other.domain &&
            type_name == other.type_name &&
            type == other.type &&
            default? == other.default? &&
            default == other.default
        end
        alias eql? ==

        def hash
          [self.class, name, domain, type_name, type, default?, default].hash
        end

        def inspect
          "#<#{self.class.name} #{name.inspect} #{declaration.join('=')} resolved=#{type.inspect}>"
        end
      end
    end
  end
end
