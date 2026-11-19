# frozen_string_literal: true

module AMQ
  module Protocol
    module Spec
      # A protocol class declaration: an ID, a name, its methods and,
      # for content-carrying classes, its content properties.
      #
      # Instances are immutable. Methods are captured in declaration (wire)
      # order; `properties` likewise, because the position of a property
      # determines its flag bit.
      class Class
        attr_reader :name, :id, :methods, :properties

        def initialize(name:, id:, methods: [], properties: [])
          unless name.is_a?(String) && !name.empty?
            raise ArgumentError, "class name must be a non-empty String, got #{name.inspect}"
          end
          raise ArgumentError, "class id must be an Integer, got #{id.inspect}" unless id.is_a?(Integer)

          @name = name
          @id = id
          @methods = methods.freeze
          @properties = properties.freeze
        end

        # Constant name used by generated code, e.g. "basic" => "Basic",
        # "channel" => "Channel".
        def constant_name
          @constant_name ||= name.split(/[- ]/).map { |part| part[0].upcase + part[1..].to_s.downcase }.join
        end

        def ruby_name
          constant_name
        end

        def [](method_name)
          method_named(method_name)
        end

        # Lookup by protocol method name ("publish" or "basic.publish") or by
        # numeric method ID. Returns nil for unknown names.
        def method_named(name)
          wanted = name.to_s
          wanted = wanted.split(".").last if wanted.include?(".")
          methods.detect { |m| m.name == wanted || m.ruby_name == wanted }
        end

        def method_with_id(method_id)
          methods.detect { |m| m.id == method_id }
        end

        def content?
          @content = methods.any?(&:content?) if @content.nil?
          @content
        end

        def to_h
          result = { "name" => name, "id" => id, "methods" => methods.map(&:to_h) }
          result["properties"] = properties.map(&:to_h) unless properties.empty?
          result
        end

        def ==(other)
          other.is_a?(Class) &&
            name == other.name &&
            id == other.id &&
            methods == other.methods &&
            properties == other.properties
        end
        alias eql? ==

        def hash
          [self.class, name, id, methods, properties].hash
        end

        def inspect
          "#<#{self.class.name} #{name} id=#{id} methods=#{methods.size} properties=#{properties.size}>"
        end
      end
    end
  end
end
