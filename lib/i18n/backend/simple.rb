# frozen_string_literal: true

require 'i18n/backend/base'

module I18n
  module Backend
    # A simple backend that reads translations from YAML files and stores them in
    # an in-memory hash. Relies on the Base backend.
    #
    # The implementation is provided by a Implementation module allowing to easily
    # extend Simple backend's behavior by including modules. E.g.:
    #
    #     module I18n::Backend::Pluralization
    #       def pluralize(*args)
    #         # extended pluralization logic
    #         super
    #       end
    #     end
    #
    #     I18n::Backend::Simple.include(I18n::Backend::Pluralization)
    class Simple
      # A Catalog is an immutable, versioned snapshot of the translations
      # stored in the backend. Publishing a new catalog (see #publish) merges
      # and validates the new data on a private copy first and then swaps the
      # catalog in atomically, so readers either see the complete old version
      # or the complete new version, never a mixture of both.
      class Catalog
        attr_reader :version, :data

        def initialize(version, data)
          @version = version
          @data    = data
        end
      end

      module Implementation
        include Base

        # Mutex to ensure that concurrent translations loading will be thread-safe
        MUTEX = Mutex.new

        def initialized?
          @initialized ||= false
        end

        # Translates the given key. The current catalog is pinned for the
        # duration of the translation, so alias resolution, pluralization and
        # interpolation all read from the same catalog version, even if a new
        # version is published concurrently.
        def translate(locale, key, options = EMPTY_HASH)
          pin_current_catalog { super }
        end

        # Stores translations for the given locale in memory.
        # This uses a deep merge for the translations hash, so existing
        # translations will be overwritten by new ones only at the deepest
        # level of the hash.
        def store_translations(locale, data, options = EMPTY_HASH)
          if I18n.enforce_available_locales &&
            I18n.available_locales_initialized? &&
            !I18n.locale_available?(locale)
            return data
          end
          locale = locale.to_sym
          store = current_catalog.data
          store[locale] ||= Concurrent::Hash.new
          data = Utils.deep_symbolize_keys(data) unless options.fetch(:skip_symbolize_keys, false)
          MUTEX.synchronize { Utils.deep_merge!(store[locale], data) }
        end

        # Publishes a new catalog version. The given +data+ (a Hash mapping
        # locales to translation data) is deep-merged onto a private copy of
        # the current catalog, the merged candidate is validated and then
        # swapped in atomically. If merging or validation fails an
        # I18n::ArgumentError is raised and the current catalog is left
        # untouched. Returns the published catalog.
        def publish(data, options = EMPTY_HASH)
          init_translations unless initialized?
          current_catalog # make sure the base catalog exists outside the mutex
          MUTEX.synchronize do
            candidate = build_catalog(data, options)
            validate_catalog!(candidate)
            swap_catalog(candidate)
          end
        end

        # The version of the catalog currently in use. While a translation is
        # in progress this is the version of the catalog pinned by it.
        def catalog_version
          (pinned_catalog || current_catalog).version
        end

        # The catalog translations are currently read from.
        def current_catalog
          @catalog || MUTEX.synchronize do
            @catalog ||= swap_catalog(Catalog.new(0, new_translation_store))
          end
        end

        # Get available locales from the translations hash
        def available_locales
          init_translations unless initialized?
          translations.inject([]) do |locales, (locale, data)|
            locales << locale unless data.size <= 1 && (data.empty? || data.has_key?(:i18n))
            locales
          end
        end

        # Clean up translations hash and set initialized to false on reload!
        def reload!
          @initialized = false
          @catalog = nil
          @translations = nil
          super
        end

        def eager_load!
          init_translations unless initialized?
          super
        end

        def translations(do_init: false)
          # To avoid returning empty translations,
          # call `init_translations`
          init_translations if do_init && !initialized?

          (pinned_catalog || current_catalog).data
        end

      protected

        def init_translations
          load_translations
          @initialized = true
        end

        # Looks up a translation from the translations hash. Returns nil if
        # either key is nil, or locale, scope or key do not exist as a key in
        # the nested translations hash. Splits keys or scopes containing dots
        # into multiple keys, i.e. <tt>currency.format</tt> is regarded the same as
        # <tt>%w(currency format)</tt>.
        def lookup(locale, key, scope = [], options = EMPTY_HASH)
          init_translations unless initialized?
          keys = I18n.normalize_keys(locale, key, scope, options[:separator])

          keys.inject(translations) do |result, _key|
            return nil unless result.is_a?(Hash)
            unless result.has_key?(_key)
              _key = _key.to_s.to_sym
              return nil unless result.has_key?(_key)
            end
            result = result[_key]
            result = resolve_entry(locale, _key, result, Utils.except(options.merge(:scope => nil), :count)) if result.is_a?(Symbol)
            result
          end
        end

        # Pins the current catalog for the duration of the block. Nested
        # translations (e.g. triggered by alias resolution) reuse the
        # already pinned catalog.
        def pin_current_catalog
          if pinned_catalog
            yield
          else
            begin
              self.pinned_catalog = current_catalog
              yield
            ensure
              self.pinned_catalog = nil
            end
          end
        end

        def pinned_catalog
          Thread.current[catalog_pin_key]
        end

        def pinned_catalog=(catalog)
          Thread.current[catalog_pin_key] = catalog
        end

        # Builds a candidate catalog by deep-merging +data+ (a Hash mapping
        # locales to translation data) onto a private copy of the current
        # catalog. The current catalog is never modified, so a failed
        # publish leaves the previous version intact.
        def build_catalog(data, options = EMPTY_HASH)
          unless data.is_a?(Hash)
            raise I18n::ArgumentError, "catalog data must be a Hash mapping locales to translation data, but is: #{data.inspect}"
          end

          prepared = {}
          data.each_pair do |locale, locale_data|
            if I18n.enforce_available_locales &&
              I18n.available_locales_initialized? &&
              !I18n.locale_available?(locale)
              next
            end
            unless locale_data.is_a?(Hash)
              raise I18n::ArgumentError, "catalog data for locale #{locale.inspect} must be a Hash, but is: #{locale_data.inspect}"
            end
            prepared[locale.to_sym] = options.fetch(:skip_symbolize_keys, false) ? locale_data : Utils.deep_symbolize_keys(locale_data)
          end

          merged = Utils.deep_merge!(deep_copy_catalog_data(current_catalog.data), prepared)
          Catalog.new(current_catalog.version + 1, merged)
        end

        # Validates a candidate catalog before it gets published. Raises
        # I18n::ArgumentError if the catalog is not well-formed.
        def validate_catalog!(catalog)
          catalog.data.each_pair do |locale, locale_data|
            unless locale.is_a?(Symbol) && locale_data.is_a?(Hash)
              raise I18n::ArgumentError, "invalid catalog entry: #{locale.inspect} => #{locale_data.inspect}"
            end
          end
          catalog
        end

        # Atomically swaps the current catalog in. Readers holding a
        # reference to a previously pinned catalog are not affected.
        def swap_catalog(catalog)
          @catalog = catalog
          @translations = catalog.data
          catalog
        end

        def new_translation_store
          Concurrent::Hash.new do |hash, key|
            MUTEX.synchronize do
              hash[key] = Concurrent::Hash.new
            end
          end
        end

        def deep_copy_catalog_data(data)
          data.each_with_object(Concurrent::Hash.new) do |(key, value), copy|
            copy[key] = value.is_a?(Hash) ? deep_copy_catalog_data(value) : value
          end
        end

        def catalog_pin_key
          @catalog_pin_key ||= :"i18n.catalog.#{object_id}"
        end
      end

      include Implementation
    end
  end
end
