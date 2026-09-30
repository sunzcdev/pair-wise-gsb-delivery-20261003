# frozen_string_literal: true

module Dry
  module Types
    # The built-in Hash type can be defined in terms of keys and associated types
    # its values can contain. Such definitions are named {Schema}s and defined
    # as lists of {Key} types.
    #
    # @see Dry::Types::Schema::Key
    #
    # {Schema} evaluates default values for keys missing in input hash
    #
    # @see Dry::Types::Default#evaluate
    # @see Dry::Types::Default::Callable#evaluate
    #
    # {Schema} implements Enumerable using its keys as collection.
    #
    # @api public
    class Schema < Hash
      NO_TRANSFORM = ::Dry::Types::FnContainer.register { |x| x }
      SYMBOLIZE_KEY = ::Dry::Types::FnContainer.register(:to_sym.to_proc)

      include ::Enumerable

      # @return [Array[Dry::Types::Schema::Key]]
      attr_reader :keys

      # @return [Hash[Symbol, Dry::Types::Schema::Key]]
      attr_reader :name_key_map

      # @return [#call]
      attr_reader :transform_key

      # @param [Class] _primitive
      # @param [Hash] options
      #
      # @option options [Array[Dry::Types::Schema::Key]] :keys
      # @option options [String] :key_transform_fn
      #
      # @api private
      def initialize(_primitive, **options)
        @keys = options.fetch(:keys)
        @name_key_map = keys.each_with_object({}) do |key, idx|
          idx[key.name] = key
        end

        key_fn = options.fetch(:key_transform_fn, NO_TRANSFORM)

        @transform_key = ::Dry::Types::FnContainer[key_fn]

        super
      end

      # @param [Hash] hash
      #
      # @return [Hash{Symbol => Object}]
      #
      # @api private
      def call_unsafe(hash, options = EMPTY_HASH)
        resolve_unsafe(coerce(hash), options)
      end

      # @param [Hash] hash
      #
      # @return [Hash{Symbol => Object}]
      #
      # @api private
      def call_safe(hash, options = EMPTY_HASH)
        resolve_safe(coerce(hash) { return yield }, options) { return yield }
      end

      # @param [Hash] hash
      #
      # @option options [Boolean] :skip_missing If true don't raise error if on missing keys
      # @option options [Boolean] :resolve_defaults If false default value
      #                                             won't be evaluated for missing key
      # @return [Hash{Symbol => Object}]
      #
      # @api public
      def apply(hash, options = EMPTY_HASH) = call_unsafe(hash, options)

      # @param input [Hash] hash
      #
      # @yieldparam [Failure] failure
      # @yieldreturn [Result]
      #
      # @return [Logic::Result]
      # @return [Object] if coercion fails and a block is given
      #
      # @api public
      # rubocop:disable Metrics/AbcSize
      # rubocop:disable Metrics/PerceivedComplexity
      def try(input)
        if primitive?(input)
          success = true
          output = {}
          result = {}

          input.each do |key, value|
            k = @transform_key.(key)
            type = @name_key_map[k]

            if type
              key_result = type.try(value)
              result[k] = key_result
              output[k] = key_result.input
              success &&= key_result.success?
            elsif strict?
              success = false
            end
          end

          if output.size < keys.size
            resolve_missing_keys(output, options) do
              success = false
            end
          end

          success &&= primitive?(output)

          if success
            failure = nil
          else
            error = CoercionError.new("#{input} doesn't conform schema", meta: result)
            failure = failure(output, error)
          end
        else
          failure = failure(input, CoercionError.new("#{input} must be a hash"))
        end

        if failure.nil?
          success(output)
        elsif block_given?
          yield(failure)
        else
          failure
        end
      end
      # rubocop:enable Metrics/AbcSize
      # rubocop:enable Metrics/PerceivedComplexity

      # @param meta [Boolean] Whether to dump the meta to the AST
      #
      # @return [Array] An AST representation
      #
      # @api public
      def to_ast(meta: true)
        [
          :schema,
          [keys.map { |key| key.to_ast(meta: meta) },
           options.slice(:key_transform_fn, :type_transform_fn, :strict, :namespace),
           meta_ast(meta)]
        ]
      end

      # Whether the schema rejects unknown keys
      #
      # @return [Boolean]
      #
      # @api public
      def strict?
        options.fetch(:strict, false)
      end

      # Make the schema intolerant to unknown keys
      #
      # @return [Schema]
      #
      # @api public
      def strict(strict = true)
        with(strict: strict)
      end

      # Inject a key transformation function
      #
      # @param [#call,nil] proc
      # @param [#call,nil] block
      #
      # @return [Schema]
      #
      # @api public
      def with_key_transform(proc = nil, &block)
        fn = proc || block

        raise ::ArgumentError, "a block or callable argument is required" if fn.nil?

        handle = ::Dry::Types::FnContainer.register(fn)
        with(key_transform_fn: handle)
      end

      # Whether the schema transforms input keys
      #
      # @return [Boolean]
      #
      # @api public
      def transform_keys?
        !options[:key_transform_fn].nil?
      end

      # @overload schema(type_map, meta = EMPTY_HASH)
      #   @param [{Symbol => Dry::Types::Nominal}] type_map
      #   @param [Hash] meta
      #   @return [Dry::Types::Schema]
      #
      # @overload schema(keys)
      #   @param [Array<Dry::Types::Schema::Key>] key List of schema keys
      #   @param [Hash] meta
      #   @return [Dry::Types::Schema]
      #
      # @api public
      def schema(keys_or_map)
        if keys_or_map.is_a?(::Array)
          new_keys = keys_or_map
        else
          new_keys = build_keys(keys_or_map)
        end

        keys = merge_keys(self.keys, new_keys)
        Schema.new(primitive, **options, keys: keys, meta: meta)
      end

      # Iterate over each key type
      #
      # @return [Array<Dry::Types::Schema::Key>,Enumerator]
      #
      # @api public
      def each(&)
        keys.each(&)
      end

      # Whether the schema has the given key
      #
      # @param [Symbol] name Key name
      #
      # @return [Boolean]
      #
      # @api public
      def key?(name)
        name_key_map.key?(name)
      end

      # Fetch key type by a key name
      #
      # Behaves as ::Hash#fetch
      #
      # @overload key(name, fallback = Undefined)
      #   @param [Symbol] name Key name
      #   @param [Object] fallback Optional fallback, returned if key is missing
      #   @return [Dry::Types::Schema::Key,Object] key type or fallback if key is not in schema
      #
      # @overload key(name, &block)
      #   @param [Symbol] name Key name
      #   @param [Proc] block Fallback block, runs if key is missing
      #   @return [Dry::Types::Schema::Key,Object] key type or block value if key is not in schema
      #
      # @api public
      def key(name, fallback = Undefined, &)
        if Undefined.equal?(fallback)
          name_key_map.fetch(name, &)
        else
          name_key_map.fetch(name, fallback)
        end
      end

      # @return [Boolean]
      #
      # @api public
      def constrained?
        true
      end

      # @return [Lax]
      #
      # @api public
      def lax
        Lax.new(schema(keys.map(&:lax)))
      end

      # Merge given schema keys into current schema
      #
      # A new instance is returned.
      #
      # @param other [Schema] schema
      # @return [Schema]
      #
      # @api public
      def merge(other)
        schema(other.keys)
      end

      # Empty schema with the same options
      #
      # @return [Schema]
      #
      # @api public
      def clear
        with(keys: EMPTY_ARRAY)
      end

      private

      # @param [Array<Dry::Types::Schema::Keys>] keys
      #
      # @return [Dry::Types::Schema]
      #
      # @api private
      def merge_keys(*keys)
        keys
          .flatten(1)
          .each_with_object({}) { |key, merged| merged[key.name] = key }
          .values
      end

      # Validate and coerce a hash. Raise an exception on any error
      #
      # @api private
      #
      # @return [Hash]
      def resolve_unsafe(hash, options = EMPTY_HASH)
        return resolve_unsafe_shared(hash, options) if options[:shared]

        resolve_unsafe_coerce(hash, options)
      end

      # Validate and coerce a hash. Call a block and halt on any error
      #
      # @api private
      #
      # @return [Hash]
      def resolve_safe(hash, options = EMPTY_HASH, &block)
        return resolve_safe_shared(hash, options, &block) if options[:shared]

        resolve_safe_coerce(hash, options, &block)
      end

      # Validate and coerce a hash into a freshly allocated container
      #
      # @api private
      #
      # @return [Hash]
      def resolve_unsafe_coerce(hash, options)
        result = {}

        hash.each do |key, value|
          k = @transform_key.(key)
          type = @name_key_map[k]

          if type
            begin
              result[k] = type.call_unsafe(value)
            rescue ConstraintError => exception
              raise SchemaError.new(type.name, value, exception.result)
            rescue CoercionError => exception
              raise SchemaError.new(type.name, value, exception.message)
            end
          elsif strict?
            raise unexpected_keys(hash.keys)
          end
        end

        resolve_missing_keys(result, options) if result.size < keys.size

        result
      end

      # Validate and coerce a hash into a freshly allocated container,
      # halting by calling the block on any error
      #
      # @api private
      #
      # @return [Hash]
      def resolve_safe_coerce(hash, options, &block)
        result = {}

        hash.each do |key, value|
          k = @transform_key.(key)
          type = @name_key_map[k]

          if type
            result[k] = type.call_safe(value, &block)
          elsif strict?
            yield
          end
        end

        resolve_missing_keys(result, options, &block) if result.size < keys.size

        result
      end

      # Validate and coerce a hash reusing the original container whenever
      # its keys, value identities, order and retention are unchanged
      #
      # @api private
      #
      # @return [Hash]
      def resolve_unsafe_shared(hash, options)
        resolve_shared(hash, options) do |type, value|
          begin
            type.call_unsafe(value, SHARED_OPTIONS)
          rescue ConstraintError => exception
            raise SchemaError.new(type.name, value, exception.result)
          rescue CoercionError => exception
            raise SchemaError.new(type.name, value, exception.message)
          end
        end
      end

      # Safe variant of {#resolve_unsafe_shared}
      #
      # @api private
      #
      # @return [Hash]
      def resolve_safe_shared(hash, options, &block)
        resolve_shared(hash, options, block) do |type, value|
          type.call_safe(value, SHARED_OPTIONS, &block)
        end
      end

      # Shared resolution shared by the safe and unsafe variants. Values
      # are coerced through the given block. As long as every key maps to
      # the same key object and every coerced value is identical (using
      # {Object#equal?}, never `==`) to the original one, and no key is
      # dropped or added, the input hash itself is returned. The first
      # divergence copies the input into a new container and all further
      # changes happen on the copy, so a failure never mutates the input.
      #
      # @api private
      #
      # @return [Hash]
      def resolve_shared(hash, options, failure = nil)
        shared = true
        result = nil

        hash.each_with_index do |(key, value), index|
          k = @transform_key.(key)
          type = @name_key_map[k]

          if type
            coerced = yield(type, value)

            if shared && k.equal?(key) && coerced.equal?(value)
              next
            end

            if shared
              result = hash.first(index).to_h
              shared = false
            end

            result[k] = coerced
          elsif strict?
            raise unexpected_keys(hash.keys)
          else
            if shared
              result = hash.first(index).to_h
              shared = false
            end
          end
        end

        return hash if shared && skip_missing_fully?(hash, options)

        if shared
          missing_result = shared_missing_result(hash, options, failure)

          return missing_result unless missing_result.nil?

          hash
        else
          resolve_missing_keys(result, options) if result.size < keys.size
          result
        end
      end

      # When missing keys are both allowed and left without defaults, a
      # hash that has every present pair unchanged can be returned as-is
      # even if it does not cover all schema keys
      #
      # @api private
      def skip_missing_fully?(hash, options)
        options.fetch(:skip_missing, false) &&
          !options.fetch(:resolve_defaults, true) &&
          hash.size < keys.size
      end

      # Resolve missing keys for a still-fully-shared hash. Returns nil
      # when sharing is still possible (nothing was added). Otherwise it
      # returns a fresh copy of the input with the missing defaults
      # inserted. A missing required key raises (unsafe) or invokes the
      # failure callback (safe), matching {#resolve_missing_keys}.
      #
      # @api private
      def shared_missing_result(hash, options, failure)
        skip_missing = options.fetch(:skip_missing, false)
        resolve_defaults = options.fetch(:resolve_defaults, true)

        result = nil

        keys.each do |key|
          next if hash.key?(key.name)

          if key.default? && resolve_defaults
            result ||= hash.dup
            result[key.name] = key.call_unsafe(Undefined, SHARED_OPTIONS)
          elsif key.required? && !skip_missing
            failure ? failure.call : raise(missing_key(key.name))
          end
        end

        result
      end

      # Try to add missing keys to the hash
      #
      # @api private
      def resolve_missing_keys(hash, options) # rubocop:disable Metrics/PerceivedComplexity
        skip_missing = options.fetch(:skip_missing, false)
        resolve_defaults = options.fetch(:resolve_defaults, true)

        keys.each do |key|
          next if hash.key?(key.name)

          if key.default? && resolve_defaults
            hash[key.name] = key.call_unsafe(Undefined)
          elsif key.required? && !skip_missing
            if block_given?
              return yield
            else
              raise missing_key(key.name)
            end
          end
        end
      end

      # @param hash_keys [Array<Symbol>]
      #
      # @return [UnknownKeysError]
      #
      # @api private
      def unexpected_keys(hash_keys)
        extra_keys = hash_keys.map(&transform_key) - name_key_map.keys
        UnknownKeysError.new(extra_keys)
      end

      # @return [MissingKeyError]
      #
      # @api private
      def missing_key(key)
        MissingKeyError.new(key)
      end
    end
  end
end
