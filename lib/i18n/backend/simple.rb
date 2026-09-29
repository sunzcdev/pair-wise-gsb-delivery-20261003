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
      # Raised when a staged catalog fails validation and can not be published.
      # The previously published catalog version stays in effect in this case.
      class InvalidCatalog < I18n::ArgumentError; end

      # An immutable, versioned snapshot of the translations hash. A single
      # translation request pins one Catalog so that alias resolution,
      # pluralization and interpolation all read from the same version.
      Catalog = Struct.new(:version, :translations)

      # An isolated, mutable view of the translation catalog. A stage starts
      # as a deep copy of the currently published catalog. Changes are merged
      # into the stage and validated without affecting live translation
      # requests, until the stage is atomically swapped in via
      # Simple#publish_catalog.
      class CatalogStage
        attr_reader :base_version, :translations

        def self.deep_copy(object)
          case object
          when Hash
            object.each_with_object({}) { |(key, value), copy| copy[key] = deep_copy(value) }
          when Array
            object.map { |element| deep_copy(element) }
          else
            object
          end
        end

        def initialize(base_version, translations)
          @base_version = base_version
          @translations = translations
          @published = false
        end

        # Merges translations for the given locale into the staged catalog.
        def store_translations(locale, data, options = EMPTY_HASH)
          raise InvalidCatalog, 'can not store translations on an already published catalog stage' if published?
          unless data.is_a?(Hash)
            raise InvalidCatalog, "translation data for #{locale.inspect} must be a Hash, but is #{data.class}"
          end
          locale = locale.to_sym
          data = Utils.deep_symbolize_keys(data) unless options.fetch(:skip_symbolize_keys, false)
          @translations[locale] = Utils.deep_merge(@translations[locale] || {}, data)
        end

        # Validates the staged catalog. Raises InvalidCatalog (or
        # I18n::InvalidLocale) when the staged data can not be published.
        def validate!
          @translations.each do |locale, data|
            unless data.is_a?(Hash)
              raise InvalidCatalog, "translation data for #{locale.inspect} must be a Hash, but is #{data.class}"
            end
            if I18n.enforce_available_locales &&
              I18n.available_locales_initialized? &&
              !I18n.locale_available?(locale)
              raise I18n::InvalidLocale.new(locale)
            end
            validate_links!(locale, data)
          end
          true
        end

        def publish!
          @published = true
        end

        def published?
          @published
        end

        protected

        def validate_links!(locale, data, path = [])
          data.each do |key, value|
            case value
            when Hash
              validate_links!(locale, value, path + [key])
            when Array
              value.each { |element| validate_link!(locale, element, path + [key]) }
            when Symbol
              validate_link!(locale, value, path + [key])
            end
          end
        end

        def validate_link!(locale, link, path)
          seen = {}
          current = link
          while current.is_a?(Symbol)
            if seen[current]
              raise InvalidCatalog, "cyclic translation alias #{current.inspect} at #{path.join('.')}"
            end
            seen[current] = true
            target = I18n.normalize_keys(locale, current, nil).inject(@translations) do |result, key|
              result.is_a?(Hash) ? result[key] : nil
            end
            if target.nil?
              raise InvalidCatalog, "translation alias #{current.inspect} at #{path.join('.')} does not resolve to an existing key"
            end
            current = target
          end
        end
      end

      module Implementation
        include Base
        
        # Mutex to ensure that concurrent translations loading will be thread-safe
        MUTEX = Mutex.new

        # Thread-local registry of catalogs pinned by in-flight translations,
        # keyed by backend instance.
        CATALOG_PINS_KEY = :i18n_simple_backend_catalog_pins

        def initialized?
          @initialized ||= false
        end

        # Pins the current catalog version for the duration of a single
        # translation, so that lookups, alias resolution, pluralization and
        # interpolation all read from the same catalog version.
        def translate(locale, key, options = EMPTY_HASH)
          with_pinned_catalog { super }
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
          translations[locale] ||= Concurrent::Hash.new
          data = Utils.deep_symbolize_keys(data) unless options.fetch(:skip_symbolize_keys, false)
          MUTEX.synchronize { Utils.deep_merge!(translations[locale], data) }
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

          @translations ||= Concurrent::Hash.new do |h, k|
            MUTEX.synchronize do
              h[k] = Concurrent::Hash.new
            end
          end
        end

        # Returns the version of the catalog that is currently in effect.
        # While a translation is in flight this is the pinned version,
        # otherwise it is the latest published version.
        def catalog_version
          catalog = pinned_catalog
          catalog ? catalog.version : current_catalog_version
        end

        # Returns the catalog that is currently in effect: the pinned catalog
        # of an in-flight translation, or the latest published one.
        def current_catalog
          pinned_catalog || MUTEX.synchronize { Catalog.new(current_catalog_version, translations) }
        end

        # Returns an isolated staging view of the current catalog. Merge new
        # translations into the stage and validate it without affecting live
        # translation requests, then publish it atomically with
        # #publish_catalog.
        def stage_catalog
          init_translations unless initialized?
          current = current_catalog
          CatalogStage.new(current.version, CatalogStage.deep_copy(current.translations))
        end

        # Validates and atomically publishes a staged catalog as a new
        # catalog version. When validation fails the previously published
        # catalog version stays in effect. Returns the new catalog version.
        def publish_catalog(stage)
          stage.validate!
          MUTEX.synchronize do
            @catalog_version = current_catalog_version + 1
            @translations = stage.translations
            @initialized = true
            stage.publish!
          end
          reset_memoizations! if respond_to?(:reset_memoizations!, true)
          @catalog_version
        end

        # Stages, validates and atomically publishes a new catalog version.
        #
        #   backend.publish_translations do |stage|
        #     stage.store_translations(:en, :foo => 'bar')
        #   end
        def publish_translations
          stage = stage_catalog
          yield stage
          publish_catalog(stage)
        end

      protected

        def init_translations
          load_translations
          @initialized = true
        end

        # Pins the current catalog for the duration of the block. Pins are
        # reentrant: nested translations (e.g. from alias resolution) reuse
        # the catalog pinned by the outermost translation.
        def with_pinned_catalog
          pins = Thread.current[CATALOG_PINS_KEY] ||= {}
          if pins.key?(object_id)
            yield
          else
            pins[object_id] = MUTEX.synchronize { Catalog.new(current_catalog_version, translations) }
            begin
              yield
            ensure
              pins.delete(object_id)
            end
          end
        end

        def pinned_catalog
          pins = Thread.current[CATALOG_PINS_KEY]
          pins && pins[object_id]
        end

        def current_catalog_version
          @catalog_version ||= 0
        end

        # Looks up a translation from the translations hash. Returns nil if
        # either key is nil, or locale, scope or key do not exist as a key in the
        # nested translations hash. Splits keys or scopes containing dots
        # into multiple keys, i.e. <tt>currency.format</tt> is regarded the same as
        # <tt>%w(currency format)</tt>.
        def lookup(locale, key, scope = [], options = EMPTY_HASH)
          init_translations unless initialized?
          keys = I18n.normalize_keys(locale, key, scope, options[:separator])
          source = (catalog = pinned_catalog) ? catalog.translations : translations

          keys.inject(source) do |result, _key|
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
      end

      include Implementation
    end
  end
end
