# frozen_string_literal: true

module Dry
  class Struct
    # Builds a new {Struct} type from an existing one by selecting a
    # subtree of its attributes. The source class is never modified and
    # default values of attributes that are not projected are never
    # evaluated.
    #
    # @see ClassInterface#project
    #
    # @api private
    class Projection
      # A selected subtree. +true+ means "keep this attribute (and, for
      # nested structs, all of its attributes)". A nested {Tree} means
      # "project this struct/array-of-struct attribute with these fields".
      Tree = ::Hash

      LEAF = true

      # @api private
      #
      # Collects selected attributes from a block passed to {ClassInterface#project}
      class TreeBuilder
        attr_reader :tree

        def initialize
          @tree = {}
        end

        # Select an attribute. Pass a block to select fields of a nested
        # struct or of elements of an array of structs.
        #
        # @param name [Symbol, String]
        def attribute(name, &block)
          key = name.to_sym
          @tree[key] =
            if block
              self.class.new.tap { |builder| builder.instance_exec(&block) }.tree
            else
              LEAF
            end
        end
        alias_method :field, :attribute

        # Allow `attrs :id, :name` style selection
        #
        # @param names [Array<Symbol, String>]
        def attributes(*names)
          names.each { |name| attribute(name) }
        end
        alias_method :fields, :attributes
      end

      # Normalize a tree given as keyword args and/or a block
      #
      # @return [Tree]
      def self.tree(selection = nil, &block)
        tree =
          case selection
          when nil then {}
          when ::Hash then normalize_hash(selection)
          else
            raise ::ArgumentError,
                  "project expects a Hash of selected fields or a block, " \
                  "#{selection.inspect} given"
          end

        if block
          block_tree = TreeBuilder.new.tap { |b| b.instance_exec(&block) }.tree
          tree = deep_merge(tree, block_tree)
        end

        tree
      end

      # @param source [Class] a Dry::Struct subclass
      # @param tree [Tree]
      # @return [Class] a new Dry::Struct subclass containing only selected fields
      def self.call(source, tree)
        new(source, tree).()
      end

      def initialize(source, tree)
        @source = source
        @tree = tree
      end

      # @return [Class]
      def call
        build_view(@source, @tree, EMPTY_ARRAY)
      end

      private

      # @param klass [Class] a Dry::Struct subclass
      # @param selection [Tree]
      # @param path [Array<Symbol>] path to this struct, for error messages
      # @return [Class] the projected struct class
      def build_view(klass, selection, path)
        if selection.empty?
          raise(
            InvalidProjection,
            "Can't build an empty projection of #{klass}" \
            "#{" at #{path.map(&:inspect).join(" -> ")}" unless path.empty?}: " \
            "select at least one attribute"
          )
        end

        projected =
          selection.map do |name, subtree|
            key = key!(klass, name, path)
            type =
              if subtree.equal?(LEAF)
                key.type
              else
                project_type(key.type, subtree, path + [name])
              end

            [key, type]
          end

        # Start from a schema with the same key/type transformations but no
        # keys, so nothing is inherited from the source schema. Register the
        # attributes inside the class body (it is evaluated in the context
        # of the new class, so attribute registration must happen here).
        ::Class.new(projection_superclass(klass)) do
          schema klass.schema.clear

          projected.each do |key, type|
            if key.required?
              attribute(key.name, type)
            else
              attribute(:"#{key.name}?", type)
            end
          end
        end
      end

      # Project a single attribute type, descending into nested structs
      # (possibly wrapped in arrays, sums/defaults/constructors).
      #
      # @param type [Object]
      # @param selection [Tree]
      # @param path [Array<Symbol>]
      def project_type(type, selection, path)
        case type
        when ::Class
          if struct_class?(type)
            build_view(type, selection, path)
          else
            cannot_descend!(type, path)
          end
        when Dry::Types::Array::Member
          type.of(project_type(type.member, selection, path))
        when Dry::Types::Default
          project_default(type, selection, path)
        when Dry::Types::Sum
          project_sum(type, selection, path)
        else
          if wrapped_type?(type)
            # Constrained, Constructor, Meta, Lax, Enum, ... — recurse into
            # the wrapped type so a struct hidden behind a constructor or a
            # constraint can still be projected.
            project_wrapped(type, selection, path)
          else
            cannot_descend!(type, path)
          end
        end
      end

      # Types that wrap another type (dry-types Decorator and Constructor)
      def wrapped_type?(type)
        type.is_a?(Dry::Types::Decorator) || type.is_a?(Dry::Types::Constructor)
      end

      # Rewrap a default type so that, when the default fires, its value is
      # coerced through the projected struct type (otherwise the original
      # nested struct, carrying unselected fields, would leak). The default
      # is still evaluated lazily and only when the key is missing.
      def project_default(default, selection, path)
        projected_inner = project_type(default.type, selection, path)
        original_value = default.value

        projected_inner.default {
          if default.callable?
            # Callable defaults receive the (projected) inner type
            original_value.call(projected_inner)
          elsif original_value.nil?
            nil
          else
            # Coerce the literal default (a struct/hash) into the view type
            projected_inner.call_unsafe(original_value)
          end
        }
      end

      # Only rewrite branches that carry a struct. The nil branch of an
      # optional type, for instance, is left untouched.
      def project_sum(sum, selection, path)
        left = contains_struct?(sum.left) ? project_type(sum.left, selection, path) : sum.left
        right = contains_struct?(sum.right) ? project_type(sum.right, selection, path) : sum.right
        sum.class.new(left, right, **sum.options)
      end

      # Does the type contain a Dry::Struct somewhere in its wrappers?
      def contains_struct?(type)
        case type
        when ::Class
          struct_class?(type)
        when Dry::Types::Array::Member
          contains_struct?(type.member)
        when Dry::Types::Sum
          contains_struct?(type.left) || contains_struct?(type.right)
        else
          wrapped_type?(type) ? contains_struct?(type.type) : false
        end
      end

      # Project the wrapped type of a generic decorator/constructor. If it
      # wraps a plain (non-struct) leaf, descending is an error.
      def project_wrapped(type, selection, path)
        projected = project_type(type.type, selection, path)
        type.send(:__new__, projected)
      end

      def key!(klass, name, path)
        key = klass.schema.key(name, nil)
        return key unless key.nil?

        raise UnknownProjectionPath.new(path: path + [name], klass: klass)
      end

      def cannot_descend!(type, path)
        raise(
          InvalidProjection,
          "Can't select nested fields from #{path.last.inspect}: " \
          "its type #{type.inspect} (at #{path.map(&:inspect).join(" -> ")}) " \
          "is neither a struct nor an array of structs"
        )
      end

      def struct_class?(type)
        type.is_a?(::Class) && type <= Struct
      end

      # Project from the same abstract base (Struct or Value) so value
      # semantics are preserved, while not inheriting any schema keys.
      def projection_superclass(klass)
        klass.respond_to?(:abstract_class) && klass.abstract_class ? klass.abstract_class : Struct
      end

      class << self
        private

        def normalize_hash(hash)
          hash.each_with_object({}) do |(name, value), normalized|
            normalized[name.to_sym] =
              case value
              when true then LEAF
              when ::Hash then normalize_hash(value)
              else
                raise ::ArgumentError,
                      "projection values must be true or a Hash, #{value.inspect} given for #{name}"
              end
          end
        end

        def deep_merge(left, right)
          left.merge(right) do |_key, lval, rval|
            if lval.is_a?(::Hash) && rval.is_a?(::Hash)
              deep_merge(lval, rval)
            else
              rval
            end
          end
        end
      end
    end
  end
end
