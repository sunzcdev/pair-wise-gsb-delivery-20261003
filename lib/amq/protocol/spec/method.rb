# frozen_string_literal: true

module AMQ
  module Protocol
    module Spec
      # A protocol method declaration. It knows the ID and name of its owning
      # class by value, so a method can be built before (or without) the
      # owning Spec::Class instance, keeping descriptions free of back
      # references.
      class Method
        attr_reader :class_name, :class_id, :name, :id, :arguments

        # class_name - owning class protocol name, e.g. "basic"
        # class_id   - owning class ID
        # name       - protocol name, e.g. "publish"
        # id         - method ID within the class
        # arguments  - ordered Array of Spec::Field
        def initialize(class_name:, class_id:, name:, id:, arguments: [],
                       content: false, synchronous: false)
          unless name.is_a?(String) && !name.empty?
            raise ArgumentError, "method name must be a non-empty String, got #{name.inspect}"
          end
          unless id.is_a?(Integer) && class_id.is_a?(Integer)
            raise ArgumentError, "method #{name.inspect} needs integer class and method ids"
          end

          @class_name = class_name.to_s
          @class_id = class_id
          @name = name
          @id = id
          @arguments = arguments.freeze
          @content = content ? true : false
          @synchronous = synchronous ? true : false
        end

        def content?
          @content
        end
        alias has_content? content?

        def synchronous?
          @synchronous
        end

        # Full protocol name, e.g. "basic.publish".
        def qualified_name
          "#{class_name}.#{name}"
        end

        # 32 bit class/method identifier using the protocol's big-endian layout.
        def index
          (class_id << 16) | id
        end

        # The packed class and method IDs as they appear at the start of a
        # method frame payload.
        def index_packed
          @index_packed ||= [class_id, id].pack("n2").freeze
        end

        def ruby_name
          @ruby_name ||= name.tr("- ", "_").to_sym
        end

        # Constant name used by generated namespaces, e.g. "publish" => "Publish".
        def constant_name
          @constant_name ||= name.split(/[- ]/).map { |part| part[0].upcase + part[1..].to_s }.join
        end

        # Ordered names of the arguments that appear on the wire.
        def argument_names
          @argument_names ||= arguments.map(&:ruby_name)
        end

        def with_class(class_name:, class_id:)
          self.class.new(
            class_name: class_name,
            class_id: class_id,
            name: name,
            id: id,
            arguments: arguments,
            content: content?,
            synchronous: synchronous?
          )
        end

        def to_h
          result = { "name" => name, "id" => id }
          result["content"] = true if content?
          result["synchronous"] = true if synchronous?
          result["arguments"] = arguments.map(&:to_h)
          result
        end

        def ==(other)
          other.is_a?(Method) &&
            class_name == other.class_name &&
            class_id == other.class_id &&
            name == other.name &&
            id == other.id &&
            content? == other.content? &&
            synchronous? == other.synchronous? &&
            arguments == other.arguments
        end
        alias eql? ==

        def hash
          [self.class, class_name, class_id, name, id, content?, synchronous?, arguments].hash
        end

        def inspect
          "#<#{self.class.name} #{qualified_name} (#{class_id}, #{id})>"
        end
      end
    end
  end
end
