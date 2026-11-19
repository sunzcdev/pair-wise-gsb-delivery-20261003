# encoding: binary
# frozen_string_literal: true

require "amq/pack"
require "amq/protocol/codec"
require "amq/protocol/frame"

module AMQ
  module Protocol
    # Base classes for generated per-version namespaces.
    #
    # A generated namespace contains one subclass of Versioned::ProtocolClass
    # per protocol class and one subclass of Versioned::Method per method.
    # Every class is bound to exactly one description, so ID lookup, argument
    # metadata and encode/decode all come from the same version. Nothing is
    # redefined in AMQ::Protocol, so versions do not overwrite each other.
    module Versioned
      # Raised when a wire value cannot be turned into a method of the
      # version's description.
      class DecodeError < StandardError; end

      # Argument list coding driven by one version's ordered field list.
      # Field order comes from the description and is never rearranged;
      # consecutive bit fields share one octet, as the protocol requires.
      module Arguments
        module_function

        # Fills positions beyond the supplied values with declared defaults.
        # Arguments without a declared default are required.
        def with_defaults(fields, values)
          return values if values.size == fields.size

          if values.size > fields.size
            raise ArgumentError,
                  "expected #{fields.size} arguments (#{fields.map(&:ruby_name).join(', ')}), got #{values.size}"
          end

          missing = fields[values.size..].reject(&:default?).map(&:ruby_name)
          unless missing.empty?
            raise ArgumentError, "missing required arguments: #{missing.join(', ')}"
          end

          values + fields[values.size..].map(&:default)
        end

        def encode(fields, values)
          values = with_defaults(fields, values)

          buffer = String.new(encoding: Encoding::BINARY)
          index = 0
          while index < fields.size
            field = fields[index]
            if field.bit?
              run = [field]
              cursor = index + 1
              while cursor < fields.size && fields[cursor].bit? && run.size < 8
                run << fields[cursor]
                cursor += 1
              end
              buffer << Codec.encode_bits(run.each_index.map { |offset| values[index + offset] })
              index = cursor
            else
              buffer << Codec.encode_value(field.type, values[index])
              index += 1
            end
          end
          buffer
        end

        def decode(fields, data)
          data = data.to_s.b
          values = []
          offset = 0
          index = 0
          while index < fields.size
            field = fields[index]
            if field.bit?
              count = 0
              while index + count < fields.size && fields[index + count].bit? && count < 8
                count += 1
              end
              bits, offset = Codec.decode_bits(data, offset, count)
              values.concat(bits)
              index += count
            else
              value, offset = Codec.decode_value(field.type, data, offset)
              values << value
              index += 1
            end
          end
          [values, offset]
        end
      end

      # Base class of a generated protocol method. Subclasses are created one
      # per method by Protocol::Registry and bound to their description.
      class Method
        class << self
          attr_reader :description, :spec

          def configure(description:, spec:)
            @description = description
            @spec = spec
            # One reader per declared argument, in declaration order.
            attr_reader(*spec.arguments.map(&:ruby_name)) unless spec.arguments.empty?
            self
          end

          def method_id
            spec.id
          end

          def class_id
            spec.class_id
          end

          def index
            spec.index
          end

          # Protocol name, e.g. "basic.publish".
          def name
            spec.qualified_name
          end

          def protocol_name
            spec.name
          end

          def argument_specs
            spec.arguments
          end

          # One reader per declared argument, in declaration order.
          def argument_names
            @argument_names ||= spec.arguments.map(&:ruby_name)
          end

          def content?
            spec.content?
          end
          alias has_content? content?

          def synchronous?
            spec.synchronous?
          end

          # Encodes the method arguments only, without a frame header.
          def encode_arguments(*values)
            Arguments.encode(spec.arguments, values)
          end

          # Decodes a method frame payload (without the frame header) into an
          # instance of this class. Rejects trailing bytes so a payload of a
          # different version does not decode silently.
          def decode(payload)
            payload = payload.to_s.b
            values, offset = Arguments.decode(spec.arguments, payload)
            if offset != payload.bytesize
              raise DecodeError, "#{name} consumed #{offset} of #{payload.bytesize} bytes"
            end
            new(*values)
          end

          # Encodes the method into a MethodFrame on `channel`.
          def encode(channel, *values)
            MethodFrame.new(spec.index_packed + encode_arguments(*values), channel)
          end
        end

        attr_reader :values

        def initialize(*values)
          expected = self.class.argument_specs.size
          if values.size != expected
            raise ArgumentError, "#{self.class.name} expects #{expected} arguments, got #{values.size}"
          end

          @values = values.freeze
          self.class.argument_specs.zip(values) do |field, value|
            instance_variable_set(:"@#{field.ruby_name}", value)
          end
        end

        # Access by argument name, e.g. method_value[:routing_key].
        def [](name)
          position = self.class.argument_names.index(name.to_s.tr("- ", "_").to_sym)
          position && values[position]
        end

        def index
          self.class.index
        end

        def has_content?
          self.class.content?
        end

        def description
          self.class.description
        end

        # Named argument hash, in declaration (wire) order.
        def to_h
          self.class.argument_specs.zip(values).each_with_object({}) do |(field, value), acc|
            acc[field.ruby_name] = value
          end
        end

        def ==(other)
          other.is_a?(Method) && other.class == self.class && other.values == values
        end

        def inspect
          "#<#{self.class.name} #{to_h.inspect}>"
        end
      end

      # Base class of a generated protocol class. Holds the methods declared
      # by the version for this class and the content property list.
      class ProtocolClass
        # One flag word covers 15 properties plus a continuation bit.
        PROPERTY_FLAGS_PER_WORD = 15

        class << self
          attr_reader :description, :spec

          def configure(description:, spec:)
            @description = description
            @spec = spec
            self
          end

          def class_id
            spec.id
          end

          def method_id
            spec.id
          end

          # Protocol class name, e.g. "basic".
          def name
            spec.name
          end

          def methods
            @methods ||= []
          end

          # Adds a method class to this class and exposes it as a constant,
          # e.g. Basic::Publish. Validates the method against this version's
          # declaration so a namespace can never mix versions.
          def register_method(method_class)
            declared = spec.method_with_id(method_class.spec.id)
            unless declared
              raise ArgumentError,
                    "#{method_class.spec.qualified_name} is not declared by #{description.version}"
            end
            unless method_class.description.equal?(description) && declared == method_class.spec
              raise ArgumentError,
                    "#{method_class.spec.qualified_name} does not match the #{description.version} declaration"
            end

            methods << method_class
            const_set(method_class.spec.constant_name, method_class)
            method_class
          end

          def [](method_name)
            wanted = method_name.to_s
            methods.detect { |m| m.spec.name == wanted || m.spec.ruby_name == wanted }
          end

          def property_specs
            spec.properties
          end

          def properties
            @properties ||= spec.properties.map(&:ruby_name).freeze
          end

          def content?
            spec.content?
          end

          # Encodes a content header payload: class ID, weight, body size,
          # flag words and property values in declaration order. Property
          # order is wire semantics, it decides the flag bits.
          def encode_properties(body_size, properties)
            unless content?
              raise ArgumentError, "#{name} does not carry content"
            end

            properties ||= {}
            unknown = properties.keys.map { |key| key.to_s.tr("- ", "_").to_sym } - property_specs.map(&:ruby_name)
            unless unknown.empty?
              raise ArgumentError, "unknown #{name} properties: #{unknown.join(', ')}"
            end
            given = properties.each_with_object({}) { |(key, value), acc| acc[key.to_s.tr("- ", "_").to_sym] = value }

            payload = String.new(encoding: Encoding::BINARY)
            payload << [spec.id, 0].pack("n2")
            payload << AMQ::Pack.pack_uint64_big_endian(body_size)

            present = property_specs.map { |field| given[field.ruby_name] }
            words, current = [], 0
            present.each_with_index do |value, index|
              bit = index % PROPERTY_FLAGS_PER_WORD
              current |= (1 << (15 - bit)) unless value.nil?
              next unless bit == PROPERTY_FLAGS_PER_WORD - 1

              words << current
              current = 0
            end
            words << current if present.size % PROPERTY_FLAGS_PER_WORD != 0 || words.empty?
            words.each_with_index do |word, index|
              last = index == words.size - 1
              word |= 1 unless last
              payload << [word].pack("n")
            end

            present.each_with_index do |value, index|
              next if value.nil?

              payload << Codec.encode_value(property_specs[index].type, value)
            end
            payload
          end

          # Decodes a content header payload back into a property hash.
          #
          # `data` starts at the property flag words, that is, after the class
          # ID, weight and body size, the same slice the legacy
          # Basic.decode_properties expects.
          def decode_properties(data)
            data = data.to_s.b
            offset = 0
            words = []
            loop do
              word = data.byteslice(offset, 2).unpack1("n")
              offset += 2
              words << word
              break if (word & 1).zero?
            end

            properties = {}
            property_specs.each_with_index do |field, index|
              word = words[index / PROPERTY_FLAGS_PER_WORD]
              bit = index % PROPERTY_FLAGS_PER_WORD
              next unless (word & (1 << (15 - bit))) != 0

              value, offset = Codec.decode_value(field.type, data, offset)
              properties[field.ruby_name] = value
            end

            if offset != data.bytesize
              raise DecodeError, "#{name} properties consumed #{offset} of #{data.bytesize} bytes"
            end

            properties
          end
        end
      end
    end
  end
end
