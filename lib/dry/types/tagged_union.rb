# frozen_string_literal: true

module Dry
  module Types
    # A union type that dispatches to exactly one member based on a tag field.
    #
    # Unlike a plain {Sum}, a TaggedUnion never tries members one by one. It
    # reads the tag from the input, looks up the corresponding member, and
    # applies only that member. A missing tag raises {MissingTagError} and an
    # unregistered tag raises {UnknownTagError}; once a member is selected its
    # failure is propagated as-is, without falling back to other members.
    #
    # The tag is read from the untouched input, before any member coercion or
    # key transformation runs, so members that are not selected never have
    # their constructors or default values evaluated.
    #
    # Lookup convention: the tag key is always declared as a symbol. A symbol
    # key in the input takes precedence over the matching string key, if both
    # are present. Tag values are matched with +eql?+ as they are, i.e. there
    # is no string/symbol coercion, so +"create"+ and +:create+ are distinct
    # tags.
    #
    # @example
    #   Event = Dry::Types::TaggedUnion.new(
    #     :kind,
    #     "create" => Dry::Types["hash"].schema(kind: Dry::Types["strict.string"]),
    #     "cancel" => Dry::Types["hash"].schema(kind: Dry::Types["strict.string"])
    #   )
    #
    #   Event.(kind: "create") # => {kind: "create"}
    #
    # @api public
    class TaggedUnion
      include Type
      include Options
      include Meta
      include Builder
      include Printable
      include ::Dry::Equalizer(:tag, :mapping, :options, :meta, inspect: false, immutable: true)

      # @return [Symbol]
      #
      # @api public
      attr_reader :tag

      # @return [Hash{Object => Type}]
      #
      # @api public
      attr_reader :mapping

      # @param [Symbol] tag Name of the field used to select a member
      # @param [Hash{Object => Type}, nil] mapping Tag value to member type.
      #   May be omitted and passed as keyword arguments instead:
      #   <code>TaggedUnion.new(:kind, "create" => create)</code>
      # @param [Hash] options
      #
      # @raise [ArgumentError] when the tag is not a symbol or the mapping is empty
      #
      # @api public
      def initialize(tag, mapping = nil, **options)
        unless tag.is_a?(::Symbol)
          raise ::ArgumentError, "tag must be a Symbol, #{tag.inspect} given"
        end

        meta = options.delete(:meta) { EMPTY_HASH }
        if mapping.nil?
          mapping = options
          options = EMPTY_HASH
        end

        unless mapping.is_a?(::Hash) && !mapping.empty?
          raise ::ArgumentError, "mapping must be a non-empty Hash"
        end

        @tag = tag
        @mapping = mapping.freeze

        super(tag, mapping, meta: meta, **options)
        freeze
      end

      # @return [String]
      #
      # @api public
      def name
        "tagged_union(#{tag.inspect})[#{mapping.keys.map(&:inspect).join(", ")}]"
      end

      # @return [false]
      #
      # @api public
      def default? = false

      # @return [false]
      #
      # @api public
      def constrained? = false

      # @return [false]
      #
      # @api public
      def optional? = false

      # @param [Object] input
      #
      # @return [Object]
      #
      # @raise [MissingTagError, UnknownTagError] when the tag is absent or unknown
      #
      # @api private
      def call_unsafe(input)
        member_for(input).call_unsafe(input)
      end

      # @param [Object] input
      #
      # @return [Object]
      #
      # @api private
      def call_safe(input, &block)
        member_for(input).call_safe(input, &block)
      rescue CoercionError
        block ? yield(input) : raise
      end

      # @param [Object] input
      #
      # @yieldparam [Result::Failure] failure
      # @yieldreturn [Object]
      #
      # @return [Result::Failure, Object]
      #
      # @api public
      def try(input, &block)
        member_for(input).try(input, &block)
      rescue CoercionError => exception
        failure = Result::Failure.new(input, exception)
        block ? yield(failure) : failure
      end

      # @param [Object] value
      #
      # @return [Boolean]
      #
      # @api private
      def primitive?(value)
        member = member_for(value)
      rescue CoercionError
        false
      else
        member.primitive?(value)
      end

      # @param [Boolean] meta Whether to dump the meta to the AST
      #
      # @return [Array]
      #
      # @api public
      def to_ast(meta: true)
        [
          :tagged_union,
          [
            tag,
            mapping.map { |value, type| [value, type.to_ast(meta: meta)] },
            meta ? self.meta : EMPTY_HASH
          ]
        ]
      end

      private

      # @param [Object] input
      #
      # @return [Type]
      #
      # @raise [CoercionError]
      #
      # @api private
      def member_for(input)
        unless input.respond_to?(:key?)
          raise CoercionError.new("#{input.inspect} must be a Hash to read #{tag.inspect} tag")
        end

        value =
          if input.key?(tag)
            input[tag]
          elsif input.key?(tag.to_s)
            input[tag.to_s]
          else
            raise MissingTagError.new(tag)
          end

        mapping.fetch(value) { raise UnknownTagError.new(tag, value, mapping.keys) }
      end
    end

    # @api private alias used by {BuilderMethods} to avoid name collision
    # with the {BuilderMethods#TaggedUnion} factory method
    TaggedUnionType = TaggedUnion
  end
end
