# frozen_string_literal: true

module I18n
  module Backend
    # Allows temporarily overriding a small set of translations for the
    # duration of a block, e.g. to serve multiple brands from one service:
    #
    #   I18n.with_overlay(:en => { :greeting => 'Welcome to Acme' }) do
    #     I18n.t(:greeting) # => "Welcome to Acme"
    #     I18n.t(:other)    # => falls through to the regular backend
    #   end
    #
    # The overlay is stored in Fiber-local state (falling back to a thread
    # variable on Rubies without fiber locals), so concurrent fibers/threads
    # each see their own overlay. The global backend is never swapped or
    # mutated, and the previous context is restored with +ensure+ even when
    # the block raises. Overlays nest: an inner +with_overlay+ is deep-merged
    # over the outer one and wins on conflicts.
    #
    # Overlay data is a hash keyed by locale, then by (possibly nested)
    # translation keys. It is snapshotted and frozen when the block is
    # entered and is never written back to any backend.
    #
    # A key in the overlay can be:
    #
    # * <b>missing</b> - the overlay has no opinion and the lookup falls
    #   through to the regular backend (and its defaults/fallbacks).
    # * <b>nil</b> - the key is explicitly blanked for this overlay. The
    #   backend value is suppressed; the lookup behaves as if the key had a
    #   +nil+ translation (i.e. +default:+ still applies, otherwise the key
    #   is reported missing). Inside a returned subtree the key stays present
    #   with a +nil+ value.
    # * <b>DELETE</b> - the key is explicitly removed for this overlay. The
    #   backend value is suppressed and the key is reported missing. Inside a
    #   returned subtree the key is removed entirely, unlike +nil+ which
    #   keeps the key with a +nil+ value.
    #
    # To enable, include the module into your backend. Include it *last*, so
    # that overlay lookups are not memoized or cached under another brand's
    # cache key:
    #
    #   I18n::Backend::Simple.include(I18n::Backend::Overlay)
    #
    # When combined with I18n::Backend::Cache, the overlay's content is mixed
    # into the cache key, so one brand's cached translations are never served
    # to another brand (or to non-overlay requests).
    module Overlay
      # Sentinel used as an overlay value to explicitly delete a key.
      DELETE = Object.new
      class << DELETE
        def inspect
          'I18n::Backend::Overlay::DELETE'
        end
        alias to_s inspect
      end

      class << self
        # Runs the block with the given translations overlaid on top of the
        # current backend (and on top of any enclosing overlay).
        def with_overlay(translations)
          raise ArgumentError, 'overlay translations must be a Hash' unless translations.is_a?(Hash)

          overlay = normalize(translations)
          overlay = deep_merge(stack.last, overlay) unless stack.empty?
          stack.push(overlay)
          begin
            yield
          ensure
            stack.pop
          end
        end

        # The effective overlay for the current fiber, or nil.
        def current
          stack.last
        end

        private

        # Fiber-local state. Child fibers inherit fiber-local variables by
        # reference (see I18n::Config#owned_by?), so the stack tracks its
        # owner and is copied on first access from any other fiber. A fiber
        # spawned inside +with_overlay+ therefore sees the parent's overlays
        # as its starting context, but can never mutate the parent's stack.
        class Stack # :nodoc:
          attr_reader :owner, :entries

          def initialize(owner, entries)
            @owner = owner
            @entries = entries
          end
        end

        def stack
          owner = Fiber.respond_to?(:[]) ? Fiber.current : Thread.current
          state = read_state

          if state.nil?
            state = Stack.new(owner, [])
            write_state(state)
          elsif state.owner != owner
            state = Stack.new(owner, state.entries.dup)
            write_state(state)
          end

          state.entries
        end

        def read_state
          if Fiber.respond_to?(:[])
            Fiber[:i18n_backend_overlay]
          else
            Thread.current.thread_variable_get(:i18n_backend_overlay)
          end
        end

        def write_state(state)
          if Fiber.respond_to?(:[])
            Fiber[:i18n_backend_overlay] = state
          else
            Thread.current.thread_variable_set(:i18n_backend_overlay, state)
          end
        end

        # Deeply symbolizes keys and freezes a private copy of the data so
        # the overlay cannot be mutated while (or after) being used.
        def normalize(data)
          data.each_with_object({}) do |(key, value), result|
            result[key.to_sym] = case value
            when Hash   then normalize(value)
            when String then value.dup.freeze
            else value
            end
          end.freeze
        end

        # Merges +overlay+ over +base+; both hashes keep their own nested
        # hashes where the other side has no opinion.
        def deep_merge(base, overlay)
          return overlay unless base
          base.merge(overlay) do |_key, old_value, new_value|
            old_value.is_a?(Hash) && new_value.is_a?(Hash) ? deep_merge(old_value, new_value) : new_value
          end.freeze
        end

        # Returns a fresh hash with all DELETE markers removed, recursively.
        def prune_deleted(hash)
          hash.each_with_object({}) do |(key, value), result|
            next if value.equal?(DELETE)
            result[key] = value.is_a?(Hash) ? prune_deleted(value) : value
          end
        end
      end

      protected

      def lookup(locale, key, scope = nil, options = EMPTY_HASH)
        overlay = Overlay.current
        return super unless overlay

        entry = overlay
        found = I18n.normalize_keys(locale, key, scope, options[:separator]).all? do |k|
          if entry.is_a?(Hash) && entry.key?(k)
            entry = entry[k]
            true
          else
            false
          end
        end

        return super unless found

        case entry
        when Hash
          backend_entry = super
          merged = backend_entry.is_a?(Hash) ? Overlay.send(:deep_merge, backend_entry, entry) : entry
          Overlay.send(:prune_deleted, merged)
        when DELETE
          nil
        else
          entry
        end
      end

      # Mixes the current overlay into the cache key so cached translations
      # are never shared between different overlays (brands) or leaked to
      # non-overlay requests.
      def cache_key(locale, key, options)
        cache_key = super
        overlay = Overlay.current
        overlay ? "#{cache_key}/overlay=#{overlay.hash}" : cache_key
      end
    end
  end
end
