# frozen_string_literal: true

# This module allows a service that hosts multiple brands (tenants) on a
# single I18n backend to temporarily override a small set of translations
# for the duration of a block, e.g.:
#
#   class Backend < I18n::Backend::Simple
#     include I18n::Backend::Cache   # optional
#     include I18n::Backend::Overlay # include last so it runs first
#   end
#
#   I18n.with_overlay(en: { greeting: 'Welcome to Brand A' }) do
#     I18n.t(:greeting) # => "Welcome to Brand A"
#     I18n.t(:tagline)  # => falls through to the regular backend
#   end
#
#   I18n.t(:greeting) # => "Hello" (backend value, nothing leaked)
#
# The overlay is a read-only, per-Fiber stack of translation hashes. It is
# *not* implemented by swapping I18n.backend and swapping it back: the
# global backend is never touched, the overlay hash is deep-frozen when it
# is pushed, and the stack is stored in fiber-local state (falling back to
# thread-local state on Rubies without fiber-local variables). The previous
# stack is restored in an +ensure+ block, so the context is restored even
# when the block raises. Note that the stack is never mutated in place -
# it is always replaced as a whole - because Ruby fibers inherit their
# parent's fiber-local variables by reference, so in-place mutation would
# leak overlays across fibers.
#
# Overlays nest: an inner +with_overlay+ block shadows the outer one for
# the keys it defines, while all other keys still resolve through the outer
# overlay and finally the backend.
#
# The top-level keys of an overlay are locales, exactly like the hashes
# passed to #store_translations, so overrides apply per locale:
#
#   I18n.with_overlay(en: { greeting: 'Howdy' }) do
#     I18n.t(:greeting, locale: :en) # => "Howdy"
#     I18n.t(:greeting, locale: :de) # => backend value
#   end
#
# == nil vs. missing vs. deleted
#
# These three states are distinct:
#
# [missing (key absent from the overlay)]
#   The overlay does not participate at all and the key resolves through
#   the regular backend, exactly as if no overlay was active.
# [nil (key present with a nil value)]
#   The overlay deliberately overrides the backend value with nil, i.e. it
#   *masks* the backend entry for exactly that key. A nil entry behaves
#   like a nil stored in the Simple backend: I18n.translate treats it as
#   missing (a :default applies, :raise raises MissingTranslationData) and
#   I18n.exists? returns false. Keys nested *below* a nil leaf are not
#   affected and still fall through to the backend.
# [deleted (key present with the I18n::Backend::Overlay::DELETED marker)]
#   An explicit tombstone. The key - and its entire subtree - is treated
#   as if it does not exist anywhere: the backend value is masked,
#   I18n.translate raises MissingTranslationData (or uses a :default) and
#   I18n.exists? returns false. Unlike nil, the marker also masks keys
#   nested below it.
#
#   I18n.with_overlay(en: { admin: I18n::Backend::Overlay::DELETED }) do
#     I18n.t(:'admin.panel.title', raise: true) # => raises MissingTranslationData
#   end
#
# == Caching
#
# When combined with I18n::Backend::Cache, the identity of the currently
# active overlay stack is mixed into the cache key, so a cached result
# produced for one brand is never served to another brand (or to a
# request running without an overlay). Overlays with equal content share
# cache entries; different overlays never do.
#
# When combined with I18n::Backend::Memoize, include this module *last*
# (as shown above) so the overlay is consulted before the memoized
# backend lookup and only backend results get memoized.
module I18n
  module Backend
    module Overlay
      # Explicit deletion marker. See the module documentation.
      DELETED = Object.new.freeze

      # Internal sentinel signalling "this overlay does not contain the key".
      NOT_FOUND = Object.new.freeze # :nodoc:

      # Shared empty stack, returned when no overlay is active.
      EMPTY_STACK = [].freeze # :nodoc:

      class << self
        # Pushes +overlays+ onto the fiber-local overlay stack for the
        # duration of the block. The hash is normalized (keys symbolized)
        # and deep-frozen, so it cannot be mutated while active. The stack
        # is replaced (never mutated in place) and restored in +ensure+,
        # so the previous context is restored even when the block raises.
        def with(overlays)
          raise ArgumentError, 'I18n.with_overlay requires a block' unless block_given?

          previous_stack = current_stack
          self.current_stack = previous_stack + [deep_freeze(Utils.deep_symbolize_keys(overlays))]
          begin
            yield
          ensure
            self.current_stack = previous_stack
          end
        end

        # The overlay stack of the current fiber (innermost overlay last).
        def current_stack
          read_stack || EMPTY_STACK
        end

        # A token identifying the currently active overlay stack, mixed
        # into cache keys so results are never shared across overlays.
        # Equal overlay content yields an equal token.
        def cache_token
          stack = current_stack
          stack.empty? ? nil : stack.map(&:hash).join('/')
        end

        private

        def read_stack
          if Fiber.respond_to?(:[])
            Fiber[:i18n_overlay_stack]
          else
            Thread.current.thread_variable_get(:i18n_overlay_stack)
          end
        end

        def current_stack=(stack)
          if Fiber.respond_to?(:[])
            Fiber[:i18n_overlay_stack] = stack
          else
            Thread.current.thread_variable_set(:i18n_overlay_stack, stack)
          end
        end

        def deep_freeze(object)
          case object
          when Hash  then object.each_value { |value| deep_freeze(value) }
          when Array then object.each { |value| deep_freeze(value) }
          end
          object.freeze
        end
      end

      protected

      # Consults the overlay stack (innermost first) before delegating to
      # the regular backend. A DELETED tombstone short-circuits to nil,
      # which Base#translate treats as a missing translation.
      def lookup(locale, key, scope = [], options = EMPTY_HASH)
        stack = Overlay.current_stack
        return super if stack.empty?

        keys = I18n.normalize_keys(locale, key, scope, options[:separator])
        stack.reverse_each do |overlay|
          result = overlay_lookup(overlay, keys)
          next if result.equal?(NOT_FOUND)
          return nil if result.equal?(DELETED)
          return result
        end
        super
      end

      # Mixes the overlay identity into the cache key when an overlay is
      # active, so one brand's cached result is never served to another.
      def cache_key(locale, key, options)
        token = Overlay.cache_token
        token ? "#{super}/overlay/#{token}" : super
      end

      private

      def overlay_lookup(overlay, keys)
        keys.inject(overlay) do |node, key|
          return NOT_FOUND unless node.is_a?(Hash) && node.has_key?(key)
          node = node[key]
          return DELETED if node.equal?(DELETED) # tombstone masks the subtree
          node
        end
      end
    end
  end
end
