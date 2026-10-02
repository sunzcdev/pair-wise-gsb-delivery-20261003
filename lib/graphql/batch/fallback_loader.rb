module GraphQL::Batch
  # Composes a primary loader with a fallback loader. Each key is first
  # loaded from the primary loader; keys for which the primary loader
  # fulfills with {MISSING} are collected and handed to the fallback loader
  # in a single batch.
  #
  # The underlying loaders keep their own caches: values from the fallback
  # loader are never written back into the primary loader's cache.
  class FallbackLoader < GraphQL::Batch::Loader
    # @param primary_loader [GraphQL::Batch::Loader]
    # @param fallback_loader [GraphQL::Batch::Loader] consulted for keys that
    #   the primary loader fulfills with {MISSING}
    # @param on_missing [:reject, #call, Object] final handling for keys that
    #   are {MISSING} from the fallback loader too. `:reject` rejects the
    #   promise with {KeyNotFoundError}, a callable is called with the key and
    #   its result fulfills the promise, and any other value fulfills every
    #   such key with that value.
    def initialize(primary_loader, fallback_loader, on_missing: :reject)
      @primary_loader = primary_loader
      @fallback_loader = fallback_loader
      @on_missing = on_missing
    end

    # Composes two loaders within the current batch.
    #
    # @see #initialize
    def self.compose(primary_loader, fallback_loader, on_missing: :reject)
      self.for(primary_loader, fallback_loader, on_missing: on_missing)
    end

    def perform(keys)
      # A single shared promise which, once the primary batch resolves,
      # enqueues every missing key with the fallback loader in one batch and
      # resolves to a {key => fallback_value} map. It rejects if the primary
      # batch fails, in which case no fallback loading is performed.
      fallback_values_promise = ::Promise
        .all(keys.map { |key| @primary_loader.load(key) })
        .then do |values|
          missing_keys = []
          values.each_with_index do |value, index|
            missing_keys << keys[index] if value.equal?(MISSING)
          end

          next {} if missing_keys.empty?

          ::Promise
            .all(missing_keys.map { |key| @fallback_loader.load(key) })
            .then do |fallback_values|
              missing_keys.zip(fallback_values).to_h
            end
        end

      keys.each do |key|
        result = @primary_loader.load(key).then do |value|
          if value.equal?(MISSING)
            fallback_values_promise.then do |fallback_values|
              resolve_missing(key, fallback_values.fetch(key))
            end
          else
            value
          end
        end
        fulfill(key, result)
      end
    end

    private

    def resolve_missing(key, value)
      return value unless value.equal?(MISSING)

      case @on_missing
      when :reject
        raise KeyNotFoundError, "no value found for key #{key.inspect}"
      else
        if @on_missing.respond_to?(:call)
          @on_missing.call(key)
        else
          @on_missing
        end
      end
    end
  end

  class Loader
    # Compose this loader with a fallback loader within the current batch.
    #
    # @see FallbackLoader#initialize
    def with_fallback(fallback_loader, on_missing: :reject)
      FallbackLoader.for(self, fallback_loader, on_missing: on_missing)
    end
  end
end
