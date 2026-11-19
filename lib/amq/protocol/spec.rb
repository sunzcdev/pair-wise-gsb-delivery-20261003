# frozen_string_literal: true

require "amq/protocol/spec/field"
require "amq/protocol/spec/class"
require "amq/protocol/spec/method"

module AMQ
  module Protocol
    # Immutable, IO-free description of one version of the AMQP protocol.
    #
    # A Description holds class/method IDs, argument and property declarations
    # and the errors the version defines. It is the source of truth for
    # ID lookups; generated namespaces (see AMQ::Protocol::Registry) and
    # encoder/decoder classes are built from it, so two versions can be
    # loaded and used side by side in one process.
    module Spec
      PRIMITIVE_TYPES = Field::PRIMITIVE_TYPES

      # Domain declarations that are not built in, e.g. "queue-name" => "shortstr".
      class Domain
        attr_reader :name, :type_name, :type

        def initialize(name:, type:, resolved_type: nil)
          raise ArgumentError, "domain name must be a String, got #{name.inspect}" unless name.is_a?(String) && !name.empty?

          @name = name
          @type_name = type
          @type = resolved_type
        end

        def to_h
          [name, type_name]
        end

        def inspect
          "#<#{self.class.name} #{name}=#{type_name}>"
        end
      end

      # A named constant such as an error code or a frame type.
      class Constant
        attr_reader :name, :value, :class_name

        def initialize(name:, value:, class_name: nil)
          @name = name
          @value = value
          @class_name = class_name
        end

        # Constant name usable in Ruby, e.g. "content-too-large" => "CONTENT_TOO_LARGE".
        def ruby_name
          @ruby_name ||= name.tr("- ", "_").upcase
        end

        def to_h
          result = { "name" => name, "value" => value }
          result["class"] = class_name if class_name
          result
        end

        def inspect
          "#<#{self.class.name} #{name}=#{value}#{class_name ? " (#{class_name})" : ''}>"
        end
      end

      # Raised when merging descriptions of one version finds two different
      # declarations under the same key. Reported instead of silently picking
      # one, so a bad description cannot be published unnoticed.
      class ConflictError < StandardError
        attr_reader :conflicts

        def initialize(conflicts, label = nil)
          @conflicts = conflicts
          header = label ? "conflicting protocol description #{label}" : "conflicting protocol description"
          super("#{header}:\n" + conflicts.map { |c| "  - #{c}" }.join("\n"))
        end
      end

      class Description
        VERSION_PATTERN = /\A(\d+)\.(\d+)\.(\d+)(?:[.-][0-9A-Za-z.-]+)?\z/

        attr_reader :label, :name, :major, :minor, :revision, :port,
                    :preamble, :domains, :constants, :classes, :errors

        # version - full version string, e.g. "0.9.1" or "0.9.1-rabbitmq"
        # name    - optional display name (defaults to "AMQP")
        def initialize(version:, name: "AMQP", port: 5672, preamble: nil,
                       domains: {}, constants: [], classes: [], label: nil)
          match = VERSION_PATTERN.match(version.to_s)
          raise ArgumentError, "invalid protocol version #{version.inspect}; expected MAJOR.MINOR.REVISION" unless match

          @label = label || version.to_s
          @name = name
          @major = Integer(match[1])
          @minor = Integer(match[2])
          @revision = Integer(match[3])
          @port = port
          @preamble = (preamble || "AMQP\x00" + [@major, @minor, @revision].pack("CCC")).b.freeze
          @domains = normalize_domains(domains).freeze
          @constants = constants.map { |c| c.is_a?(Constant) ? c : Constant.new(**symbolize(c)) }.freeze
          @classes = classes.map { |k| k.is_a?(Class) ? resolve_class(k) : raise(ArgumentError, "unsupported class declaration #{k.inspect}") }.freeze
          @errors = @constants.select(&:class_name).freeze

          @by_class_id = {}
          @by_method_id = {}
          @by_class_name = {}
          @classes.each do |klass|
            @by_class_id[klass.id] = klass
            @by_class_name[klass.name] = klass
            klass.methods.each { |m| @by_method_id[m.index] = m }
          end
          @by_class_id.freeze
          @by_method_id.freeze
          @by_class_name.freeze
        end

        def version
          @version ||= "#{major}.#{minor}.#{revision}"
        end

        def to_s
          version
        end
        def class_with_id(class_id)
          @by_class_id[class_id]
        end

        def class_named(name)
          @by_class_name[name.to_s]
        end

        def method_with_id(class_id, method_id)
          @by_method_id[(class_id << 16) | method_id]
        end

        # Lookup by the packed 32 bit class/method index used on the wire.
        def method_at_index(index)
          @by_method_id[index]
        end

        # Ordered method list, in class then method ID order as declared.
        def methods
          @methods ||= @classes.flat_map(&:methods).freeze
        end
        alias all_methods methods

        def method_classes
          @classes.select(&:content?)
        end

        # Resolves a domain or primitive type name to a primitive type.
        def resolve_domain(type_name)
          seen = []
          current = type_name
          while current && !PRIMITIVE_TYPES.include?(current)
            raise ArgumentError, "unknown protocol domain #{type_name.inspect} for #{self}" if seen.include?(current)

            seen << current
            domain = @domains[current]
            raise ArgumentError, "unknown protocol domain #{current.inspect} for #{self}" if domain.nil?

            current = domain.type_name
          end
          current
        end

        def error_for_code(code)
          @errors.detect { |e| e.value == code }
        end

        def to_h
          {
            "name" => name,
            "major-version" => major,
            "minor-version" => minor,
            "revision" => revision,
            "port" => port,
            # Domains are a name to type mapping, but declaration order is kept
            # so a description serializes back the way it was written.
            "domains" => @domains.values.map(&:to_h),
            "constants" => constants.map(&:to_h),
            "classes" => classes.map(&:to_h)
          }
        end

        # Structural fingerprint used to detect conflicting descriptions and
        # to assert deterministic generation.
        #
        # Hash keys are canonicalized; array order is preserved because
        # class, method, argument and property order are wire semantics.
        # The value is a plain string, so it does not depend on object
        # identity or string encoding.
        def digest
          @digest ||= canonical(to_h)
        end

        def ==(other)
          other.is_a?(Description) && digest == other.digest
        end
        alias eql? ==

        def hash
          digest.hash
        end

        def inspect
          "#<#{self.class.name} #{version} classes=#{classes.size} methods=#{methods.size}>"
        end

        private

        def normalize_domains(domains)
          result = {}
          entries =
            case domains
            when nil then []
            when Hash then domains.to_a
            when Array then domains
            else raise ArgumentError, "unsupported domain table #{domains.inspect}"
            end

          entries.each do |entry|
            key, value =
              case entry
              when Array then [entry[0], entry[1]]
              when Domain then [entry.name, entry]
              else raise ArgumentError, "unsupported domain declaration #{entry.inspect}"
              end

            domain =
              case value
              when Domain then value
              when Array then Domain.new(name: value[0].to_s, type: value[1].to_s)
              when String, Symbol then Domain.new(name: key.to_s, type: value.to_s)
              when Hash
                attrs = value.each_with_object({}) { |(k, v), acc| acc[k.to_s.tr("_", "-")] = v }
                Domain.new(name: key.to_s, type: (attrs["type"] || attrs["domain"]).to_s)
              else raise ArgumentError, "unsupported domain declaration #{value.inspect}"
              end

            existing = result[domain.name]
            if existing && existing.type_name != domain.type_name
              raise ArgumentError,
                    "domain #{domain.name.inspect} is declared as both #{existing.type_name.inspect} and #{domain.type_name.inspect}"
            end
            result[domain.name] = domain
          end
          result
        end

        # Fields keep the type name exactly as declared; the resolved
        # primitive type is bound here, from this description's domain table.
        def resolve_class(klass)
          methods = klass.methods.map do |method|
            Spec::Method.new(
              class_name: klass.name,
              class_id: klass.id,
              name: method.name,
              id: method.id,
              arguments: method.arguments.map { |f| resolve_field(f) },
              content: method.content?,
              synchronous: method.synchronous?
            )
          end

          Spec::Class.new(
            name: klass.name,
            id: klass.id,
            methods: methods,
            properties: klass.properties.map { |f| resolve_field(f) }
          )
        end

        def resolve_field(field)
          field.with_type(resolve_domain(field.declaration[1].to_s))
        end

        # Canonicalizes hash key order only. Arrays (declaration order) are
        # left untouched. The result is a string so equal descriptions
        # compare equal regardless of object identity.
        def canonical(value, out = nil)
          out ||= +""
          case value
          when Hash
            out << "{"
            value.keys.sort_by(&:to_s).each do |key|
              out << key.to_s.inspect << ":"
              canonical(value[key], out)
              out << ","
            end
            out << "}"
          when Array
            out << "["
            value.each do |item|
              canonical(item, out)
              out << ","
            end
            out << "]"
          when String
            out << value.b.inspect
          when Symbol
            out << value.to_s.inspect
          else
            out << value.inspect
          end
          out
        end

        def symbolize(hash)
          hash.each_with_object({}) { |(k, v), acc| acc[k.to_s.tr("-", "_").to_sym] = v }
        end
      end
    end
  end
end
