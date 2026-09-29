# frozen_string_literal: true

require "dry/validation/constants"

module Dry
  module Validation
    class Contract
      # Contract's class interface
      #
      # @see Contract
      #
      # @api public
      module ClassInterface
        include Macros::Registrar

        # @api private
        def inherited(klass)
          super
          klass.instance_variable_set("@config", config.dup)
        end

        # Configuration
        #
        # @example
        #   class MyContract < Dry::Validation::Contract
        #     config.messages.backend = :i18n
        #   end
        #
        # @return [Config]
        #
        # @api public
        def config
          @config ||= Validation::Config.new
        end

        # Return macros registered for this class
        #
        # @return [Macros::Container]
        #
        # @api public
        def macros
          config.macros
        end

        # Define a params schema for your contract
        #
        # This type of schema is suitable for HTTP parameters
        #
        # @return [Dry::Schema::Params,NilClass]
        # @see https://dry-rb.org/gems/dry-schema/params/
        #
        # @api public
        def params(*external_schemas, &)
          define(:Params, external_schemas, &)
        end

        # Define a JSON schema for your contract
        #
        # This type of schema is suitable for JSON data
        #
        # @return [Dry::Schema::JSON,NilClass]
        # @see https://dry-rb.org/gems/dry-schema/json/
        #
        # @api public
        def json(*external_schemas, &)
          define(:JSON, external_schemas, &)
        end

        # Define a plain schema for your contract
        #
        # This type of schema does not offer coercion out of the box
        #
        # @return [Dry::Schema::Processor,NilClass]
        # @see https://dry-rb.org/gems/dry-schema/
        #
        # @api public
        def schema(*external_schemas, &)
          define(:schema, external_schemas, &)
        end

        # Define a rule for your contract
        #
        # @example using a symbol
        #   rule(:age) do
        #     failure('must be at least 18') if values[:age] < 18
        #   end
        #
        # @example using a path to a value and a custom predicate
        #   rule('address.street') do
        #     failure('please provide a valid street address') if valid_street?(values[:street])
        #   end
        #
        # @return [Rule]
        #
        # @api public
        def rule(*keys, &block)
          ensure_valid_keys(*keys) if __schema__

          Rule.new(keys: keys, block: block).tap do |rule|
            rules << rule
          end
        end

        # Mount a sub-contract for a nested value
        #
        # The sub-contract is applied to the value found under the given path
        # after the parent's schema and rules were applied. Its output replaces
        # the value at the mount path and its errors are re-pathed under the
        # mount path (base errors are attached to the mount path itself).
        #
        # @example single object
        #   mount :address, AddressContract.new
        #
        # @example array of objects
        #   mount :contacts, ContactContract.new, each: true
        #
        # @example passing the parent's context to the sub-contract
        #   mount :address, AddressContract.new, context: true
        #
        # @example passing only specific context keys to the sub-contract
        #   mount :address, AddressContract.new, context: [:current_user]
        #
        # @param [Symbol, String, Array] path Path at which the sub-contract is mounted
        # @param [Contract, Class<Contract>] contract A contract instance or class
        # @param [Boolean] each Apply the contract to each element of an array
        # @param [Boolean, Array<Symbol>, Hash] context Context passing declaration.
        #   When `false` (default) the sub-contract receives no parent context,
        #   when `true` it receives a snapshot of the parent's context, when an
        #   Array it receives only the listed keys and when a Hash the hash
        #   itself is used as the initial context. Context writes never leak
        #   back into the parent or other mount points.
        #
        # @return [Mount]
        #
        # @api public
        def mount(path, contract, each: false, context: false)
          contract = contract.new if contract.is_a?(::Class)

          unless contract.is_a?(Contract)
            raise ::ArgumentError, <<~STR.strip
              +contract+ must be a Dry::Validation::Contract (#{contract.inspect} given)
            STR
          end

          Mount.new(
            path: Schema::Path[path].to_a,
            contract: contract,
            each: each,
            context: context
          ).tap do |mount|
            mounts << mount
          end
        end

        # A shortcut that can be used to define contracts that won't be reused or inherited
        #
        # @example
        #   my_contract = Dry::Validation::Contract.build do
        #     params do
        #       required(:name).filled(:string)
        #     end
        #   end
        #
        #   my_contract.call(name: "Jane")
        #
        # @return [Contract]
        #
        # @api public
        def build(options = EMPTY_HASH, &)
          Class.new(self, &).new(**options)
        end

        # @api private
        def __schema__
          @__schema__ if defined?(@__schema__)
        end

        # Return rules defined in this class
        #
        # @return [Array<Rule>]
        #
        # @api private
        def rules
          @rules ||= EMPTY_ARRAY
            .dup
            .concat(superclass.respond_to?(:rules) ? superclass.rules : EMPTY_ARRAY)
        end

        # Return mounts defined in this class
        #
        # @return [Array<Mount>]
        #
        # @api private
        def mounts
          @mounts ||= EMPTY_ARRAY
            .dup
            .concat(superclass.respond_to?(:mounts) ? superclass.mounts : EMPTY_ARRAY)
        end

        # Return messages configured for this class
        #
        # @return [Dry::Schema::Messages]
        #
        # @api private
        def messages
          @messages ||= Schema::Messages.setup(config.messages)
        end

        private

        # @api private
        def ensure_valid_keys(*keys)
          valid_paths = key_map.to_dot_notation
          key_paths = key_paths(keys)

          invalid_keys = key_paths.filter_map { |(key, path)|
            if valid_paths.none? { |vp|
                 vp == path || vp.start_with?("#{path}.", "#{path}[]")
               }
              key
            end
          }.uniq

          return if invalid_keys.empty?

          raise InvalidKeysError, <<~STR.strip
            #{name}.rule specifies keys that are not defined by the schema: #{invalid_keys.inspect}
          STR
        end

        # @api private
        def key_paths(keys)
          keys.map { |key|
            case key
            when Hash
              path = Schema::Path[key]
              if path.multi_value?
                *head, tail = Array(path)
                [key].product(
                  tail.map { |el| [*head, *el] }.map { |parts| parts.join(DOT) }
                )
              else
                [[key, path.to_a.join(DOT)]]
              end
            when Array
              [[key, Schema::Path[key].to_a.join(DOT)]]
            else
              [[key, key.to_s]]
            end
          }.flatten(1)
        end

        # @api private
        def key_map
          __schema__.key_map
        end

        # @api private
        def core_schema_opts
          {parent: superclass&.__schema__, config: config}
        end

        # @api private
        def define(method_name, external_schemas, &block)
          return __schema__ if external_schemas.empty? && block.nil?

          unless __schema__.nil?
            raise ::Dry::Validation::DuplicateSchemaError, "Schema has already been defined"
          end

          schema_opts = core_schema_opts

          schema_opts.update(parent: external_schemas) if external_schemas.any?

          case method_name
          when :schema
            @__schema__ = Schema.define(**schema_opts, &block)
          when :Params
            @__schema__ = Schema.Params(**schema_opts, &block)
          when :JSON
            @__schema__ = Schema.JSON(**schema_opts, &block)
          end
        end
      end
    end
  end
end
