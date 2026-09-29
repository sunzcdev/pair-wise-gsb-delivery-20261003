# frozen_string_literal: true

# Memoize module simply memoizes the values returned by lookup using
# a flat hash and can tremendously speed up the lookup process in a backend.
#
# To enable it you can simply include the Memoize module to your backend:
#
#   I18n::Backend::Simple.include(I18n::Backend::Memoize)
#
# Notice that it's the responsibility of the backend to define whenever the
# cache should be cleaned.
module I18n
  module Backend
    module Memoize
      def available_locales
        @memoized_locales ||= super
      end

      def store_translations(locale, data, options = EMPTY_HASH)
        reset_memoizations!(locale)
        super
      end

      def publish(data, options = EMPTY_HASH)
        super.tap { reset_memoizations! }
      end

      def reload!
        reset_memoizations!
        super
      end

      def eager_load!
        memoized_lookup
        available_locales
        super
      end

      protected

        def lookup(locale, key, scope = nil, options = EMPTY_HASH)
          flat_key  = I18n::Backend::Flatten.normalize_flat_keys(locale,
            key, scope, options[:separator])
          # Memoized values are scoped to the catalog version so that a
          # translation pinned to an older version can neither read nor
          # pollute the memoizations of a freshly published version.
          version   = memoize_catalog_version
          flat_key  = "#{version}##{flat_key}" if version
          flat_key  = flat_key.to_sym
          flat_hash = memoized_lookup[locale.to_sym]
          flat_hash.key?(flat_key) ? flat_hash[flat_key] : (flat_hash[flat_key] = super)
        end

        def memoize_catalog_version
          catalog_version if respond_to?(:catalog_version)
        end

        def memoized_lookup
          @memoized_lookup ||= I18n.new_double_nested_cache
        end

        def reset_memoizations!(locale=nil)
          @memoized_locales = nil
          (locale ? memoized_lookup[locale.to_sym] : memoized_lookup).clear
        end
    end
  end
end
