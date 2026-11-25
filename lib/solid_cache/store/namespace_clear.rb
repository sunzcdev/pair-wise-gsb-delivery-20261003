# frozen_string_literal: true

module SolidCache
  class Store
    # Namespace-scoped, point-in-time cache clears.
    #
    # A scoped clear works in two phases per shard:
    #
    #   1. A generation marker for the namespace is bumped. Every write that
    #      completes from that point on is stamped with the new generation.
    #   2. The entries that still carry the previous generation are swept.
    #
    # Because the sweep only deletes the versions that belonged to the clear
    # scope, keys written (or rewritten) while the sweep is running survive and
    # stay readable, while a sibling namespace is never touched.
    module NamespaceClear
      private
        def scoped_clear?(options)
          options.is_a?(Hash) && options.key?(:namespace) && !options[:namespace].nil?
        end

        def resolve_clear_namespace(options)
          namespace = options[:namespace]
          namespace = self.options[:namespace] if namespace == true
          raise ArgumentError, "A namespace must be given for a scoped cache clear" if namespace.nil?

          namespace
        end

        def clear_namespace(namespace, gate:, sweep_pending:)
          cutoff_at = Time.now
          key_prefix = scoped_key_prefix(namespace)
          report = { namespace: namespace, cutoff_at: cutoff_at, shards: {} }

          connection_names.each do |shard|
            report[:shards][shard] =
              clear_namespace_on_shard(shard, namespace, key_prefix, gate, sweep_pending)
          end

          report
        end

        def clear_namespace_on_shard(shard, namespace, key_prefix, gate, sweep_pending)
          with_connection(shard) do
            failsafe(:clear_namespace, returning: { generation: 0, cleared_generation: 0, deleted: 0, failed: true }) do
              generation_lock(shard).synchronize do
                if sweep_pending
                  # Finish a clear whose generation bump already happened, but whose
                  # sweep was gated. The marker holds the first generation that must
                  # survive, so everything below it still needs to be swept. When no
                  # clear ever happened there is nothing pending on this shard.
                  generation = Generation.current(namespace)
                  if generation.zero?
                    next { generation: 0, cleared_generation: 0, deleted: 0 }
                  end

                  cleared_generation = generation - 1
                else
                  cleared_generation = Generation.current(namespace)
                  generation = Generation.bump(namespace)
                end

                if gate.nil? || gate.call(shard)
                  deleted =
                    if key_prefix
                      Entry.delete_scoped(key_prefix, cleared_generation)
                    else
                      # A store without a namespace writes unprefixed keys; there
                      # is nothing to distinguish its keys from other unprefixed
                      # keys, so the generation filter alone defines the scope.
                      Entry.delete_generation(cleared_generation)
                    end
                  { generation: generation, cleared_generation: cleared_generation, deleted: deleted }
                else
                  # The gate runs inside the lock so that a write observed by the
                  # gate is guaranteed to be stamped with the new generation.
                  { generation: generation, cleared_generation: cleared_generation, deleted: 0, paused: true }
                end
              end
            end
          end
        end

        # Builds the prefix of the fully namespaced keys as stored in the
        # entries table, e.g. "tenant-123:%". The trailing separator guarantees
        # a namespace cannot match a sibling with the same string prefix.
        # Returns nil for the default (unprefixed) namespace, whose keys carry
        # no namespace prefix at all.
        def scoped_key_prefix(namespace)
          return nil if namespace == Generation::DEFAULT_NAMESPACE

          escaped = namespace.gsub(ESCAPE_CHARS) { |char| "#{ESCAPE_CHARACTER}#{char}" }
          "#{escaped}#{KEY_PREFIX_SEPARATOR}#{WILDCARD_CHARACTER}"
        end

        KEY_PREFIX_SEPARATOR = ":"
        WILDCARD_CHARACTER = "%"
        ESCAPE_CHARACTER = "\\"
        ESCAPE_CHARS = /[\\%_]/
    end
  end
end
