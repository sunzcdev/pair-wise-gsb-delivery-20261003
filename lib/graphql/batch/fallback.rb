module GraphQL::Batch
  # Composes a primary loader with a fallback loader.
  #
  # Keys are first loaded through the primary loader. Keys that the primary
  # loader reports as missing by fulfilling them with
  # {GraphQL::Batch::Loader::Missing} (typically via `fulfill_missing`) are
  # collected and handed to the fallback loader in a single batch. A value of
  # `nil` returned by the primary loader is treated as a legitimate value and
  # does not trigger the fallback.
  #
  # If the primary loader rejects or raises, the rejection is passed through and
  # the fallback loader is never consulted. Keys still missing after the
  # fallback loader resolves are handled by the optional `on_missing` block,
  # which receives the key and may return a final value or raise. Without a
  # block, such keys resolve to `nil`.
  #
  # Neither the primary nor the fallback loader cache is mutated by this object,
  # and values produced by the fallback loader are never written back into the
  # primary loader's cache.
  class Fallback
    def initialize(primary_loader, fallback_loader, &on_missing)
      @primary_loader = primary_loader
      @fallback_loader = fallback_loader
      @on_missing = on_missing || ->(key) { nil }
    end

    def load(key)
      compose(@primary_loader.load(key), key)
    end

    def load_many(keys)
      ::Promise.all(keys.map { |key| load(key) })
    end

    # Compose another fallback loader, which is consulted only when this
    # fallback loader also reports the key as missing.
    def fallback(other_loader, &on_missing)
      chained_fallback =
        if @fallback_loader.is_a?(Fallback)
          @fallback_loader.fallback(other_loader, &on_missing)
        else
          Fallback.new(@fallback_loader, other_loader, &on_missing)
        end
      Fallback.new(@primary_loader, chained_fallback, &@on_missing)
    end

    private

    def compose(promise, key)
      promise.then(
        ->(value) do
          if value.equal?(Loader::Missing)
            @fallback_loader.load(key).then do |fallback_value|
              if fallback_value.equal?(Loader::Missing)
                @on_missing.call(key)
              else
                fallback_value
              end
            end
          else
            value
          end
        end,
      )
    end
  end

  class Loader
    # Returns a composable object that first tries this loader and then falls
    # back to `other_loader` for keys this loader reports as missing.
    def fallback(other_loader, &on_missing)
      Fallback.new(self, other_loader, &on_missing)
    end
  end
end
