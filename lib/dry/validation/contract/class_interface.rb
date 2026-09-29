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
        # @example annotating the rule with the purposes it serves
        #   rule(:terms, purposes: :submit) do
        #     key.failure('must be accepted') unless values[:terms]
        #   end
        #
        # @example declaring a cross-purpose dependency
        #   rule(:total, purposes: :submit, depends_on: :line_items) do
        #     key.failure('does not add up') if values[:total] < 0
        #   end
        #
        # @param [Hash] options Purpose annotations in a trailing options
        #   hash. Recognized keys are `:purposes`, `:depends_on` and `:name`;
        #   every other key is treated as a rule key path.
        #
        # @return [Rule]
        #
        # @api public
        def rule(*keys, &block)
          keys, options = extract_rule_options(keys)
          purposes = options.fetch(:purposes, EMPTY_ARRAY)
          depends_on = options.fetch(:depends_on, EMPTY_ARRAY)
          name = options[:name]

          ensure_valid_keys(*keys) if __schema__

          Rule.new(
            keys: keys,
            block: block,
            name: name,
            purposes: Array(purposes).flatten.map(&:to_sym),
            depends_on: Array(depends_on).flatten.map(&:to_sym)
          ).tap do |rule|
            rules << rule
          end
        end

        # Register business purposes recognized by this contract
        #
        # `includes:` declares that selecting a purpose also selects another one.
        # This is the explicit, declarative way to bundle mandatory rules
        # (for example, accepting terms is always checked when submitting).
        #
        # @example
        #   purposes :draft
        #   purposes :submit, includes: :terms_check
        #
        # @return [Array<Symbol>] Registered purpose names
        #
        # @api public
        def purposes(*names, includes: EMPTY_ARRAY)
          names = names.flatten.map(&:to_sym)
          included = Array(includes).flatten.map(&:to_sym)

          included.each do |included_name|
            registered_purposes << included_name unless registered_purposes.include?(included_name)
          end

          names.each do |name|
            registered_purposes << name unless registered_purposes.include?(name)
            purpose_includes[name] = ((purpose_includes[name] || EMPTY_ARRAY) + included).uniq
          end

          registered_purposes.dup
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

        # Purposes registered for this contract class
        #
        # Each class keeps its own copy so that registering new purposes on a
        # subclass never mutates the parent contract.
        #
        # @return [Array<Symbol>]
        #
        # @api private
        def registered_purposes
          @registered_purposes ||= begin
            parent = superclass.respond_to?(:registered_purposes) ? superclass.registered_purposes : EMPTY_ARRAY
            parent.dup
          end
        end

        # Mapping from a purpose to the purposes it implicitly includes
        #
        # @return [Hash<Symbol, Array<Symbol>>]
        #
        # @api private
        def purpose_includes
          @purpose_includes ||= begin
            parent = superclass.respond_to?(:purpose_includes) ? superclass.purpose_includes : EMPTY_HASH
            parent.each_with_object({}) { |(key, value), hash| hash[key] = value.dup }
          end
        end

        # Compile the rule selection for a purpose combination
        #
        # The result is cached per class and per purpose combination, so
        # repeated calls with different combinations never share state.
        #
        # @param [Array<Symbol>, Symbol, nil] requested_purposes
        #
        # @return [RuleSelection]
        #
        # @api private
        def compile_rules(requested_purposes)
          key = requested_purposes.nil? ? nil : Array(requested_purposes).flatten.map(&:to_sym).uniq.sort

          @rule_selection_cache ||= {}
          @rule_selection_cache[key] ||= RuleCompiler.new(self).compile(requested_purposes)
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

        # Reserved keys understood by `.rule`; everything else is a rule key
        #
        # @api private
        RULE_OPTION_KEYS = %i[purposes depends_on name].freeze

        # Separate purpose/dependency options from rule keys
        #
        # A trailing hash is only treated as an options hash when all of its
        # keys are reserved option names, so `rule(address: :zip)` keeps being a
        # rule key as before.
        #
        # @api private
        def extract_rule_options(keys)
          options = EMPTY_HASH

          if keys.last.is_a?(::Hash) &&
             (keys.last.keys - RULE_OPTION_KEYS).empty? &&
             (keys.last.keys & RULE_OPTION_KEYS).any?
            options = keys.pop
          end

          [keys, options]
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
