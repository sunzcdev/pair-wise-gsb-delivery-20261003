# frozen_string_literal: true

module Dry
  class Struct
    # Builds projected "view" classes from an attribute tree.
    #
    # A view is a brand new {Dry::Struct} subclass containing only the
    # selected attributes. Selected keys keep their original types, so
    # required/optional flags, default values and key transformations are
    # preserved. The source class is never modified, and default values of
    # attributes that were not selected are never evaluated.
    #
    # @see ClassInterface#project
    #
    # @api private
    module Projection
      class << self
        # @param [Class] source struct class to project
        # @param [Array<Symbol, Hash>] spec attribute tree
        # @param [Array<Symbol>] path current path (used in error messages)
        # @return [Class] new {Dry::Struct} subclass
        def call(source, spec, path: Core::Constants::EMPTY_ARRAY)
          view = ::Class.new(Struct)
          view.attributes(attribute_map(source, spec, path))
          inherit_key_transform(view, source)
          view
        end

        private

        def attribute_map(source, spec, path)
          each_entry(spec).each_with_object({}) do |(name, sub), map|
            key = fetch_key(source, name, path)
            type = sub.nil? ? key.type : project_nested(key.type, sub, path + [name])

            map[key.required? ? name : :"#{name}?"] = type
          end
        end

        def each_entry(spec, &block)
          return enum_for(:each_entry, spec) unless block

          spec.each do |item|
            case item
            when ::Hash
              item.each { |name, sub| block.call(name.to_sym, normalize_sub(sub)) }
            when ::Symbol, ::String
              block.call(item.to_sym, nil)
            else
              raise ProjectionError,
                    "invalid projection spec #{item.inspect}, " \
                    "use attribute names and hashes of nested selections"
            end
          end
        end

        def normalize_sub(sub)
          case sub
          when ::Array then sub
          when ::Hash, ::Symbol, ::String then ::Kernel.Array(sub)
          else
            raise ProjectionError,
                  "invalid nested projection #{sub.inspect}, " \
                  "use an array of attribute names"
          end
        end

        def fetch_key(source, name, path)
          unless source.has_attribute?(name)
            raise UnknownAttributeError.new(source: source, name: name, path: path)
          end

          source.schema.key(name)
        end

        def project_nested(type, sub, path)
          raise EmptyProjectionError, path if sub.empty?

          case type
          when ::Class
            return call(type, sub, path: path) if type <= Struct
          when Types::Sum
            return project_optional(type, sub, path)
          when Types::Default
            raise UnsupportedProjectionError.new(
              path, type,
              reason: "it has a default value bound to #{type.type}, " \
                      "select the attribute as a whole to keep its default"
            )
          else
            if struct_array?(type)
              return type.of(call(type.member, sub, path: path))
            end
          end

          raise UnsupportedProjectionError.new(path, type)
        end

        def project_optional(type, sub, path)
          left, right = type.left, type.right

          if nil_type?(left)
            project_nested(right, sub, path).optional
          elsif nil_type?(right)
            project_nested(left, sub, path).optional
          else
            raise UnsupportedProjectionError.new(path, type)
          end
        end

        def nil_type?(type)
          type.respond_to?(:primitive) && type.primitive.equal?(::NilClass)
        end

        def struct_array?(type)
          type.respond_to?(:member) &&
            type.member.is_a?(::Class) &&
            type.member <= Struct
        end

        def inherit_key_transform(view, source)
          source_schema = source.schema
          source_schema = source_schema.type if source_schema.respond_to?(:type)

          return unless source_schema.respond_to?(:transform_keys?) && source_schema.transform_keys?

          view.schema view.schema.with_key_transform(source_schema.transform_key)
        end
      end
    end
  end
end
