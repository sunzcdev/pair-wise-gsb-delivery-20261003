# frozen_string_literal: true
#--
# typed: true

module GraphQL
  module Execution
    class Interpreter
      # A container for metadata regarding arguments present in a GraphQL query.
      # See [Interpreter::Arguments#argument_values](rdoc-ref:Interpreter::Arguments#argument_values) for a hash of these objects.
      class ArgumentValue
        # The client didn't provide a value for this argument,
        # so the schema-defined `default_value:` was used.
        DEFAULT_VALUE_SOURCE = :default_value

        # The client explicitly provided `null` for this argument.
        # The resulting Ruby value may be `nil`, but this was the
        # client's decision rather than a missing value.
        NULL_VALUE_SOURCE = :null

        # The client explicitly provided a value for this argument
        # (including a value that happens to equal the configured default).
        PROVIDED_VALUE_SOURCE = :provided

        def initialize(definition:, value:, original_value:, default_used: nil, source: nil)
          @definition = definition
          @value = value
          @original_value = original_value
          if source
            @source = source
            @default_used = source == DEFAULT_VALUE_SOURCE
          else
            # Backwards-compatible path: derive the source from the boolean flag.
            @default_used = default_used
            @source = if default_used
              DEFAULT_VALUE_SOURCE
            elsif original_value.nil?
              NULL_VALUE_SOURCE
            else
              PROVIDED_VALUE_SOURCE
            end
          end
        end

        # **Returns**
        #
        # - `Object` — The Ruby-ready value for this Argument
        attr_reader :value

        # **Returns**
        #
        # - `Object` — The value of this argument _before_ `prepare` is applied.
        attr_reader :original_value

        # **Returns**
        #
        # - `GraphQL::Schema::Argument` — The definition instance for this argument
        attr_reader :definition

        # Where the {#value} of this argument came from. Unlike {#value}, this
        # distinguishes inputs that end up with the same Ruby value:
        #
        # - {DEFAULT_VALUE_SOURCE} — no client value was present, so the schema-defined `default_value:` was applied.
        # - {NULL_VALUE_SOURCE} — the client explicitly provided `null`.
        # - {PROVIDED_VALUE_SOURCE} — the client explicitly provided a value, even if it equals the default.
        #
        # **Returns**
        #
        # - `Symbol` — one of {DEFAULT_VALUE_SOURCE}, {NULL_VALUE_SOURCE}, or {PROVIDED_VALUE_SOURCE}
        #
        # :call-seq:
        #   source() -> Symbol
        attr_reader :source

        # **Returns**
        #
        # - `Boolean` — `true` if the schema-defined `default_value:` was applied in this case. (No client-provided value was present.)
        #
        # :call-seq:
        #   default_used?() -> bool
        def default_used?
          @default_used
        end

        # **Returns**
        #
        # - `Boolean` — `true` if the client explicitly provided `null` for this argument.
        #
        # :call-seq:
        #   null?() -> bool
        def null?
          @source == NULL_VALUE_SOURCE
        end

        # **Returns**
        #
        # - `Boolean` — `true` if the client explicitly provided a value for this argument
        #   (this is also `true` when the provided value equals the configured default).
        #
        # :call-seq:
        #   provided?() -> bool
        def provided?
          @source == PROVIDED_VALUE_SOURCE
        end
      end
    end
  end
end
