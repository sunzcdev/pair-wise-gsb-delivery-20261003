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
        # @example naming a rule and declaring dependencies on other rules
        #   rule(:account, name: :account_active) do
        #     key.failure('is not active') unless active?(values[:account])
        #   end
        #
        #   rule(:account, name: :account_limit, depends_on: :account_active) do
        #     key.failure('limit exceeded') if over_limit?(values[:account])
        #   end
        #
        # @param [Symbol, nil] name Optional unique name used as a dependency target
        # @param [Symbol, Array<Symbol>] depends_on Names of rules that must pass first
        #
        # @return [Rule]
        #
        # @api public
        def rule(*keys, **options, &block)
          name = options.delete(:name)
          depends_on = Array(options.delete(:depends_on) || EMPTY_ARRAY)
          keys << options unless options.empty?

          ensure_valid_keys(*keys) if __schema__
          ensure_valid_rule_dependencies(name, depends_on)

          Rule.new(keys: keys, name: name, depends_on: depends_on, block: block).tap do |rule|
            rules << rule
            @rule_schedule = nil
          end
        end

        # Return rules in a stable order that respects declared dependencies
        #
        # Rules are scheduled in definition order as long as their dependencies
        # have been scheduled before them.
        #
        # @return [Array<Rule>]
        #
        # @api private
        def rule_schedule
          @rule_schedule ||= begin
            scheduled = []
            remaining = rules.dup

            until remaining.empty?
              scheduled_names = scheduled.filter_map(&:name)
              ready_index = remaining.find_index { |rule|
                (rule.depends_on - scheduled_names).empty?
              }

              if ready_index.nil?
                raise RuleDependencyCycleError,
                      "#{self}.rule dependencies form a cycle: " \
                      "#{remaining.filter_map(&:name).inspect}"
              end

              scheduled << remaining.delete_at(ready_index)
            end

            scheduled
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
        def ensure_valid_rule_dependencies(name, depends_on)
          names = rules.filter_map(&:name)

          if name && names.include?(name)
            raise DuplicateRuleNameError,
                  "#{self}.rule name #{name.inspect} is already used by another rule"
          end

          depends_on.each do |dependency|
            if dependency == name
              raise RuleDependencyCycleError,
                    "#{self}.rule #{name.inspect} depends on itself"
            end

            next if names.include?(dependency)

            raise UnknownRuleDependencyError,
                  "#{self}.rule #{name.inspect} depends on an unknown rule #{dependency.inspect}"
          end
        end

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
