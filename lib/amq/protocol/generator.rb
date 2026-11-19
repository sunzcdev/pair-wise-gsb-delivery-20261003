# encoding: binary
# frozen_string_literal: true

require "amq/protocol/spec"
require "amq/protocol/spec/loader"

module AMQ
  module Protocol
    # Turns a protocol Description into Ruby source for a version namespace.
    #
    # Generation is pure and deterministic: for one description it always
    # emits the same bytes, so regenerating an unchanged description is a
    # no-op diff. Declarations are emitted in the order the description
    # stores them; field order is never sorted or otherwise rearranged
    # because it is part of the wire format.
    #
    # The emitted file embeds the description document as plain data and
    # registers it, so the class/method IDs, argument lists and codecs it
    # exposes all belong to that one version.
    module Generator
      module_function

      # Renders a version namespace as Ruby source.
      #
      # module_name - constant path to define, e.g. "AMQ::Protocol::Versions::V0_9_1"
      def generate(description, module_name)
        parts = module_name.to_s.split("::")
        raise ArgumentError, "module name must be a constant path" if parts.empty?

        document = description.to_h
        body = []
        body << "# encoding: binary"
        body << "# frozen_string_literal: true"
        body << ""
        body << "# Generated from the AMQP #{description.version} protocol description."
        body << "# Do not edit; regenerate with AMQ::Protocol::Generator."
        body << ""
        body << 'require "amq/protocol/registry"'
        body << ""
        parts[0..-2].each_with_index { |part, index| body << ("  " * index) + "module #{part}" }
        base = "  " * (parts.size - 1)
        inner = base + "  "
        body << "#{base}module #{parts.last}"

        body << "#{inner}# Declared field and argument order is wire semantics."
        body << "#{inner}DOCUMENT = AMQ::Protocol::Spec::Loader.freeze_document(#{ruby_literal(document)})"
        body << ""
        body << "#{inner}DESCRIPTION = AMQ::Protocol::Spec::Loader.from_document(DOCUMENT, label: #{description.label.inspect})"
        body << "#{inner}VERSION = AMQ::Protocol::Registry.register(DESCRIPTION, label: #{description.label.inspect})"
        body << "#{inner}NAMESPACE = VERSION.namespace"
        body << "#{inner}PREAMBLE = DESCRIPTION.preamble"
        body << "#{inner}PORT = DESCRIPTION.port"
        body << ""
        body << "#{inner}# Sugar for the generated protocol classes of this version."
        description.classes.each do |klass|
          body << "#{inner}#{klass.constant_name} = NAMESPACE::#{klass.constant_name}"
        end
        body << ""
        body << "#{base}end"
        (parts.size - 2).downto(0) do |index|
          body << ("  " * index) + "end"
        end
        body.join("\n") + "\n"
      end

      # Deterministic metadata dump of a description, used by release checks
      # and by tests asserting that repeated generation is stable.
      def manifest(description)
        lines = []
        lines << "version #{description.version}"
        lines << "preamble #{description.preamble.unpack1('H*')}"
        lines << "port #{description.port}"
        description.classes.each do |klass|
          lines << "class #{klass.name} #{klass.id}"
          klass.methods.each do |method|
            args = method.arguments.map { |f| "#{f.name}:#{f.type}" }.join(" ")
            lines << "  method #{method.name} #{method.id} content=#{method.content?} #{args}"
          end
          klass.properties.each_with_index do |field, index|
            lines << "  property #{index} #{field.name}:#{field.type}"
          end
        end
        description.constants.each do |constant|
          lines << "constant #{constant.name} #{constant.value}#{constant.class_name ? " #{constant.class_name}" : ''}"
        end
        lines.join("\n") + "\n"
      end

      # Canonical Ruby literal: hash keys in a fixed order, arrays as given.
      def ruby_literal(value, indent = 0)
        case value
        when Hash then hash_literal(value, indent)
        when Array then array_literal(value, indent)
        when String then value.inspect
        when Symbol then value.inspect
        when nil, true, false, Integer, Float then value.inspect
        else raise ArgumentError, "cannot emit #{value.class} as a Ruby literal"
        end
      end

      HASH_KEY_ORDER = %w[
        name major-version minor-version revision port
        domains constants classes
        id arguments methods properties content synchronous
        type domain default-value value class
      ].freeze

      def hash_literal(hash, indent)
        return "{}" if hash.empty?

        pad = "  " * (indent + 1)
        closing = "  " * indent
        keys = hash.keys
        ordered = HASH_KEY_ORDER.select { |k| keys.include?(k) } + (keys - HASH_KEY_ORDER)
        lines = ordered.map do |key|
          "#{pad}#{key.inspect} => #{ruby_literal(hash[key], indent + 1)}"
        end
        "{\n#{lines.join(",\n")}\n#{closing}}"
      end

      def array_literal(array, indent)
        return "[]" if array.empty?

        pad = "  " * (indent + 1)
        closing = "  " * indent
        lines = array.map { |item| "#{pad}#{ruby_literal(item, indent + 1)}" }
        "[\n#{lines.join(",\n")}\n#{closing}]"
      end
    end
  end
end
