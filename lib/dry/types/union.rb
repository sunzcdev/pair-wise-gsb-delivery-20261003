# frozen_string_literal: true

require "dry/core/equalizer"
require "dry/types/options"
require "dry/types/meta"

module Dry
  module Types
    # A tagged (a.k.a. discriminated) union dispatches the input to exactly one
    # of its branches based on the value of a dedicated field, the *tag*.
    #
    # Unlike a {Sum} type, a Union never tries its branches from left to right:
    #
    # * the tag is read from the *raw* input hash before any branch is applied,
    #   therefore no branch constructor or default value is ever evaluated for
    #   an input that does not carry its tag;
    # * once a branch has been selected, a coercion failure is reported
    #   directly, it never falls through to another branch;
    # * a missing tag or a tag value without a matching branch result in
    #   dedicated {MissingUnionTagError} / {UnknownUnionTagError} errors that
    #   can be told apart from branch coercion failures.
    #
    # == Tag lookup and key transformations
    #
    # Tag reading always happens on the untouched input and *precedes* any key
    # transformation performed by the branches themselves. Both the symbol
    # (+:type+) and string (+"type"+) forms of the tag field are accepted and
    # resolve to the very same branch; when both are present the symbol field
    # wins. This guarantees string-keyed and symbol-keyed payloads cannot end
    # up selecting different branches. Tag values, on the other hand, are
    # matched literally (+eql?+), no coercion is applied to them.
    #
    # @example
    #   module Types
    #     include Dry.Types()
    #
    #     Create = Types::Hash.schema(
    #       type: Types::String.constrained(eql: "create"),
    #       order_id: Types::Coercible::Integer
    #     )
    #     Cancel = Types::Hash.schema(
    #       type: Types::String.constrained(eql: "cancel"),
    #       reason: Types::String
    #     )
    #
    #     Command = Types.Union(:type, "create" => Create, "cancel" => Cancel)
    #
    #     Command.call(type: "create", order_id: "42")
    #     # => {type: "create", order_id: 42}
    #     Command.call({})
    #     # => raises Dry::Types::MissingUnionTagError
    #     Command.call(type: "pause")
    #     # => raises Dry::Types::UnknownUnionTagError
    #
    # @api public
    class Union
      include Type
      include Builder
      include Options
      include Meta
      include Printable
      include ::Dry::Equalizer(
        :tag, :mapping, :options, :meta,
        inspect: false,
        immutable: true
      )

      # @return [Symbol] name of the field used for dispatching
      attr_reader :tag

      # @return [Hash{Object => Type}] tag value to branch type mapping
      attr_reader :mapping

      # @param [Hash{Object => Type}] mapping tag value to branch type mapping
      # @param [Symbol] tag name of the field used for dispatching
      # @param [Hash] options
      #
      # @raise [ArgumentError] when the mapping is not a non-empty hash of types
      #
      # @api private
      def initialize(mapping, tag:, **options)
        unless mapping.is_a?(::Hash) && !mapping.empty?
          raise ::ArgumentError,
            "a Union requires a non-empty hash of tag => type mappings, " \
            "#{mapping.inspect} given"
        end

        mapping.each do |label, type|
          unless type.is_a?(Type)
            raise ::ArgumentError,
              "branch for tag #{label.inspect} must be a Dry::Types::Type, " \
              "#{type.inspect} given"
          end
        end

        @tag = tag.to_sym
        @mapping = mapping.freeze
        super(mapping, **options)
        freeze
      end

      # Rebuild the union keeping its identity (mapping and tag) while only
      # replacing options/metadata. The tag lives outside of +options+ and the
      # mapping is positional, hence the default Options/Meta behaviour cannot
      # reconstruct a Union.
      #
      # @api private
      def with(**new_options)
        if new_options.empty?
          self
        else
          meta = new_options.delete(:meta) { @meta }
          self.class.new(mapping, tag: tag, **options, **new_options, meta: meta)
        end
      end

      # @return [Array<Type>] the declared branch types in declaration order
      #
      # @api public
      def branches = mapping.values

      # @return [String]
      #
      # @api public
      def name
        listed = mapping.map { |label, type| "#{label.inspect} => #{type.name}" }.join(", ")
        "Union(tag: #{tag.inspect}, #{listed})"
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

      # Dispatch the input to its branch and apply the branch without rescuing
      # errors. Tag errors and branch errors alike propagate to the caller.
      #
      # @param [Object] input
      #
      # @return [Object]
      #
      # @api private
      def call_unsafe(input)
        resolve_branch(input).call_unsafe(input)
      end

      # Dispatch the input to its branch and apply the branch in safe mode.
      # Any failure (unreadable tag or branch failure) calls the given block.
      #
      # @param [Object] input
      #
      # @return [Object]
      #
      # @api private
      def call_safe(input, &block)
        branch =
          begin
            resolve_branch(input)
          rescue UnionTagError
            return yield(input)
          end

        branch.call_safe(input, &block)
      end

      # Dispatch the input to its branch and return a {Result}. A branch
      # failure is reported as-is, without trying other branches.
      #
      # @param [Object] input
      #
      # @yieldparam [Result::Failure] failure
      # @yieldreturn [Object]
      #
      # @return [Result::Success, Result::Failure] when no block is given
      # @return [Object] otherwise
      #
      # @api public
      def try(input, &block)
        branch =
          begin
            resolve_branch(input)
          rescue UnionTagError => error
            return dispatch_failure(Result::Failure.new(input, error), &block)
          end

        branch.try(input) do |failure|
          dispatch_failure(failure, &block)
        end
      end

      # @param [Object] value
      #
      # @return [Boolean]
      #
      # @api private
      def primitive?(value)
        branches.any? { |type| type.primitive?(value) }
      end

      # A lax union keeps dispatching by the tag but makes every branch lax,
      # so branch coercion failures fall back to the original input instead of
      # raising. Unreadable / unknown tags still fail.
      #
      # @return [Union]
      #
      # @api public
      def lax
        self.class.new(mapping.transform_values(&:lax), tag: tag).with(**options, meta: meta)
      end

      # Return the AST representation
      #
      # @param meta [Boolean] whether to include metadata
      #
      # @return [Array]
      #
      # @api public
      def to_ast(meta: true)
        [
          :union,
          [
            tag,
            mapping.map { |label, type| [label, type.to_ast(meta: meta)] },
            meta ? self.meta : EMPTY_HASH
          ]
        ]
      end

      # Wrap the type with a proc
      #
      # @return [Proc]
      #
      # @api public
      def to_proc = proc { |value| call(value) }

      private

      # @param [Object] input
      #
      # @return [Type]
      #
      # @raise [InvalidUnionInputError] when the input is not a hash
      # @raise [MissingUnionTagError] when no tag field is present
      # @raise [UnknownUnionTagError] when the tag value has no branch
      #
      # @api private
      def resolve_branch(input)
        mapping.fetch(read_tag(input)) do |value|
          raise UnknownUnionTagError.new(tag, value, input, mapping.keys)
        end
      end

      # Read the tag from the raw input. Symbol keys take precedence over
      # string keys but both always identify the same branch.
      #
      # @param [Object] input
      #
      # @return [Object]
      #
      # @api private
      def read_tag(input)
        unless input.is_a?(::Hash)
          raise InvalidUnionInputError.new(tag, input)
        end

        if input.key?(tag)
          input.fetch(tag)
        elsif input.key?(tag.to_s)
          input.fetch(tag.to_s)
        else
          raise MissingUnionTagError.new(tag, input)
        end
      end

      # @param [Result::Failure] failure
      #
      # @return [Object]
      #
      # @api private
      def dispatch_failure(failure)
        if block_given?
          yield(failure)
        else
          failure
        end
      end
    end
  end
end
