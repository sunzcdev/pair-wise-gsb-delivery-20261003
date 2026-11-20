# frozen_string_literal: true

require "active_support/core_ext/file/atomic"

module ActiveRecord
  module ConnectionAdapters
    # A schema cache dump that holds a separate {SchemaCache} entry for every
    # target database, keyed by the target's identity (the database
    # configuration name). This keeps databases that share a dump file but own
    # different tables, columns and indexes isolated from each other: an entry
    # is only ever handed back to the reflection bound to the same target.
    class MultiSchemaCache
      MARKER = "multi_schema_cache" # :nodoc:

      def initialize(caches = nil)
        @caches = caches || {}
      end

      def [](target)
        @caches[target]
      end

      def []=(target, cache)
        @caches[target] = cache
      end

      def key?(target)
        @caches.key?(target)
      end

      def targets
        @caches.keys
      end

      def empty?
        @caches.empty?
      end

      def encode_with(coder) # :nodoc:
        coder[MARKER] = true
        coder["caches"] = @caches.sort.to_h
      end

      def init_with(coder) # :nodoc:
        @caches = coder["caches"] || {}
        self
      end

      def as_schema_json # :nodoc:
        data = {}
        encode_with(data)
        data
      end

      def init_from_schema_json(coder, references) # :nodoc:
        @caches = (coder["caches"] || {}).transform_values { |reference| references[reference] }
        self
      end

      def marshal_dump # :nodoc:
        [MARKER, @caches]
      end

      def marshal_load(array) # :nodoc:
        _marker, @caches = array
        @caches ||= {}
      end

      def dump_to(filename)
        open_dump(filename) { |f|
          if filename.include?(".dump")
            f.write(Marshal.dump(self))
          elsif filename.include?(".json")
            f.write(JSONSchemaCacheSerializer.dump(self))
          else
            f.write(YAML.dump(self))
          end
        }
      end

      private
        def open_dump(filename)
          FileUtils.mkdir_p(File.dirname(filename))

          File.atomic_write(filename) do |file|
            if File.extname(filename) == ".gz"
              zipper = Zlib::GzipWriter.new file
              zipper.mtime = 0
              yield zipper
              zipper.flush
              zipper.close
            else
              yield file
            end
          end
        end
    end

    class SchemaReflection
      class << self
        attr_accessor :use_schema_cache_dump
        attr_accessor :check_schema_cache_dump_version
      end

      self.use_schema_cache_dump = true
      self.check_schema_cache_dump_version = true

      # ==== Parameters
      #
      # * +cache_path+ - Path of the schema cache dump file.
      # * +cache+ - An already loaded {SchemaCache} to serve directly.
      # * +target+ - Identity of the database this reflection is bound to
      #   (usually the database configuration name). Named reflections only read
      #   and write the matching entry of a {MultiSchemaCache}; +nil+ keeps the
      #   historical single-database behavior.
      def initialize(cache_path, cache = nil, target: nil)
        @cache = cache
        @cache_path = cache_path
        @target = target
        @container = nil
      end

      def clear!
        @cache = empty_cache

        # Only the entry for the database this reflection represents is reset;
        # entries belonging to other targets stay untouched on disk and in any
        # already loaded container.
        @container&.[]=(@target, @cache) if @target

        nil
      end

      def load!(pool)
        cache(pool)

        self
      end

      def primary_keys(pool, table_name)
        cache(pool).primary_keys(pool, table_name)
      end

      def data_source_exists?(pool, name)
        cache(pool).data_source_exists?(pool, name)
      end

      def add(pool, name)
        cache(pool).add(pool, name)
      end

      def data_sources(pool, name)
        cache(pool).data_source_exists?(pool, name)
      end

      def columns(pool, table_name)
        cache(pool).columns(pool, table_name)
      end

      def columns_hash(pool, table_name)
        cache(pool).columns_hash(pool, table_name)
      end

      def columns_hash?(pool, table_name)
        cache(pool).columns_hash?(pool, table_name)
      end

      def indexes(pool, table_name)
        cache(pool).indexes(pool, table_name)
      end

      def version(pool)
        cache(pool).version(pool)
      end

      def size(pool)
        cache(pool).size
      end

      def clear_data_source_cache!(pool, name)
        return if @cache.nil? && !possible_cache_available?

        cache(pool).clear_data_source_cache!(pool, name)
      end

      def cached?(table_name)
        if @cache.nil?
          # If `check_schema_cache_dump_version` is enabled we can't load
          # the schema cache dump without connecting to the database.
          unless self.class.check_schema_cache_dump_version
            @cache = load_cache(nil)
          end
        end

        @cache&.cached?(table_name)
      end

      def dump_to(pool, filename)
        fresh_cache = empty_cache
        fresh_cache.add_all(pool)

        store_cache(filename, fresh_cache)
        @cache = fresh_cache
      end

      private
        def empty_cache
          new_cache = SchemaCache.allocate
          new_cache.send(:initialize)
          new_cache
        end

        def cache(pool)
          @cache ||= load_cache(pool) || empty_cache
        end

        def possible_cache_available?
          self.class.use_schema_cache_dump &&
            @cache_path &&
            File.file?(@cache_path)
        end

        # Persist +fresh_cache+ for the target this reflection represents. An
        # existing dump is preserved entry by entry so warming up one database
        # never overwrites the entries of the others sharing the same file.
        def store_cache(filename, fresh_cache)
          container = load_container(filename)
          container = MultiSchemaCache.new if container.nil? || legacy_single_cache?(container)

          if @target
            container[@target] = fresh_cache
            container.dump_to(filename)
          elsif container.is_a?(MultiSchemaCache) && !container.empty?
            raise ArgumentError,
              "Cannot dump a single-database cache into the multi-database dump #{filename} without a target"
          else
            # Historical behavior: unnamed reflections write a bare SchemaCache.
            fresh_cache.dump_to(filename)
          end
        end

        def load_container(filename)
          return unless File.file?(filename)

          SchemaCache._load_from(filename)
        end

        def legacy_single_cache?(object)
          object.is_a?(SchemaCache)
        end

        def load_cache(pool)
          # Can't load if schema dumps are disabled
          return unless possible_cache_available?

          # Check we can find one
          return unless loaded = SchemaCache._load_from(@cache_path)

          new_cache = extract_target_cache(loaded)
          return unless new_cache

          if self.class.check_schema_cache_dump_version
            begin
              pool.with_connection do |connection|
                current_version = connection.schema_version

                if new_cache.version(connection) != current_version
                  warn "Ignoring the #{@target || "default"} entry in #{@cache_path} because it has expired. " \
                    "Current schema version is #{current_version}, but the cache file has #{new_cache.schema_version}."
                  return
                end
              end
            rescue ActiveRecordError => error
              warn "Failed to validate the schema cache because of #{error.class}: #{error.message}"
              return
            end
          end

          new_cache
        end

        # Resolve the entry belonging to this reflection's target out of the
        # loaded dump, applying a clear compatibility decision for historical
        # single-database cache files.
        def extract_target_cache(loaded)
          if loaded.is_a?(MultiSchemaCache)
            @container = loaded
            loaded[@target]
          elsif @target.nil?
            # Backwards compatibility: a legacy bare SchemaCache is only accepted
            # by an unnamed reflection, which itself represents a single database.
            loaded
          else
            warn "Ignoring #{@cache_path} because it contains a legacy single-database schema cache and " \
              "cannot be assigned to the #{@target} database. Re-dump the schema cache to upgrade it."
            nil
          end
        end
    end

    class BoundSchemaReflection
      class FakePool # :nodoc
        def initialize(connection)
          @connection = connection
        end

        def with_connection
          yield @connection
        end
      end

      class << self
        def for_lone_connection(abstract_schema_reflection, connection) # :nodoc:
          new(abstract_schema_reflection, FakePool.new(connection))
        end
      end

      def initialize(abstract_schema_reflection, pool)
        @schema_reflection = abstract_schema_reflection
        @pool = pool
      end

      def clear!
        @schema_reflection.clear!
      end

      def load!
        @schema_reflection.load!(@pool)
      end

      def cached?(table_name)
        @schema_reflection.cached?(table_name)
      end

      def primary_keys(table_name)
        @schema_reflection.primary_keys(@pool, table_name)
      end

      def data_source_exists?(name)
        @schema_reflection.data_source_exists?(@pool, name)
      end

      def add(name)
        @schema_reflection.add(@pool, name)
      end

      def data_sources(name)
        @schema_reflection.data_sources(@pool, name)
      end

      def columns(table_name)
        @schema_reflection.columns(@pool, table_name)
      end

      def columns_hash(table_name)
        @schema_reflection.columns_hash(@pool, table_name)
      end

      def columns_hash?(table_name)
        @schema_reflection.columns_hash?(@pool, table_name)
      end

      def indexes(table_name)
        @schema_reflection.indexes(@pool, table_name)
      end

      def version
        @schema_reflection.version(@pool)
      end

      def size
        @schema_reflection.size(@pool)
      end

      def clear_data_source_cache!(name)
        @schema_reflection.clear_data_source_cache!(@pool, name)
      end

      def dump_to(filename)
        @schema_reflection.dump_to(@pool, filename)
      end
    end

    # Active Record Connection Adapters Schema Cache
    # ==============================================
    class SchemaCache
      def self._load_from(filename) # :nodoc:
        return unless File.file?(filename)

        read(filename) do |file|
          if filename.include?(".dump")
            Marshal.load(file, freeze: true)
          elsif filename.include?(".json")
            JSONSchemaCacheSerializer.load(file)
          else
            YAML.unsafe_load(file)
          end
        end
      end

      def self.read(filename, &block)
        if File.extname(filename) == ".gz"
          Zlib::GzipReader.open(filename) { |gz|
            yield gz.read
          }
        elsif filename.include?(".dump")
          # Marshal output is binary. On Windows a plain File.read runs in text
          # mode and treats the 0x1A byte as end-of-file, truncating the stream.
          yield File.binread(filename)
        else
          yield File.read(filename)
        end
      end
      private_class_method :read

      def initialize # :nodoc:
        @columns      = {}
        @columns_hash = {}
        @primary_keys = {}
        @data_sources = {}
        @indexes      = {}
        @version = nil
      end

      def initialize_dup(other) # :nodoc:
        super
        @columns      = @columns.dup
        @columns_hash = @columns_hash.dup
        @primary_keys = @primary_keys.dup
        @data_sources = @data_sources.dup
        @indexes      = @indexes.dup
      end

      def encode_with(coder) # :nodoc:
        coder["columns"]          = @columns.sort.to_h
        coder["primary_keys"]     = @primary_keys.sort.to_h
        coder["data_sources"]     = @data_sources.sort.to_h
        coder["indexes"]          = @indexes.sort.to_h
        coder["version"]          = @version
      end

      def init_with(coder) # :nodoc:
        @columns          = coder["columns"]
        @columns_hash     = coder["columns_hash"]
        @primary_keys     = coder["primary_keys"]
        @data_sources     = coder["data_sources"]
        @indexes          = coder["indexes"] || {}
        @version          = coder["version"]

        unless coder["deduplicated"]
          derive_columns_hash_and_deduplicate_values
        end
      end

      def as_schema_json
        data = {}
        encode_with(data)
        data["data_sources"] = data["data_sources"].select { |_, exists| exists }.keys
        data
      end

      def init_from_schema_json(coder, references)
        @columns          = coder["columns"].transform_values { |columns| columns.map { |i| references[i] } }
        @columns_hash     = @columns.transform_values { |columns| columns.index_by(&:name) }
        @primary_keys     = coder["primary_keys"].dup
        @data_sources     = coder["data_sources"].index_with { true }
        @indexes          = (coder["indexes"] || {}).transform_values { |indexes| indexes.map { |i| references[i] } }
        @version          = coder["version"]
      end

      def cached?(table_name)
        @columns.key?(table_name)
      end

      def primary_keys(pool, table_name)
        @primary_keys.fetch(table_name) do
          pool.with_connection do |connection|
            if data_source_exists?(pool, table_name)
              @primary_keys[deep_deduplicate(table_name)] = deep_deduplicate(connection.primary_key(table_name))
            end
          end
        end
      end

      # A cached lookup for table existence.
      def data_source_exists?(pool, name)
        return if ignored_table?(name)

        if @data_sources.empty?
          tables_to_cache(pool).each do |source|
            @data_sources[source] = true
          end
        end

        return @data_sources[name] if @data_sources.key? name

        @data_sources[deep_deduplicate(name)] = pool.with_connection do |connection|
          connection.data_source_exists?(name)
        end
      end

      # Add internal cache for table with `table_name`.
      def add(pool, table_name)
        pool.with_connection do
          if data_source_exists?(pool, table_name)
            primary_keys(pool, table_name)
            columns(pool, table_name)
            columns_hash(pool, table_name)
            indexes(pool, table_name)
          end
        end
      end

      # Get the columns for a table
      def columns(pool, table_name)
        if ignored_table?(table_name)
          raise ActiveRecord::StatementInvalid.new("Table '#{table_name}' doesn't exist", connection_pool: pool)
        end

        @columns.fetch(table_name) do
          pool.with_connection do |connection|
            @columns[deep_deduplicate(table_name)] = deep_deduplicate(connection.columns(table_name))
          end
        end
      end

      # Get the columns for a table as a hash, key is the column name
      # value is the column object.
      def columns_hash(pool, table_name)
        @columns_hash.fetch(table_name) do
          @columns_hash[deep_deduplicate(table_name)] = columns(pool, table_name).index_by(&:name).freeze
        end
      end

      # Checks whether the columns hash is already cached for a table.
      def columns_hash?(_pool, table_name)
        @columns_hash.key?(table_name)
      end

      def indexes(pool, table_name)
        @indexes.fetch(table_name) do
          pool.with_connection do |connection|
            if data_source_exists?(pool, table_name)
              @indexes[deep_deduplicate(table_name)] = deep_deduplicate(connection.indexes(table_name))
            else
              []
            end
          end
        end
      end

      def version(pool)
        @version ||= pool.with_connection(&:schema_version)
      end

      def schema_version
        @version
      end

      def size
        [@columns, @columns_hash, @primary_keys, @data_sources].sum(&:size)
      end

      # Clear out internal caches for the data source `name`.
      def clear_data_source_cache!(_connection, name)
        @columns.delete name
        @columns_hash.delete name
        @primary_keys.delete name
        @data_sources.delete name
        @indexes.delete name
      end

      def add_all(pool) # :nodoc:
        pool.with_connection do |connection|
          tables = tables_to_cache(pool)

          tables.each { |table| @data_sources[deep_deduplicate(table)] = true }

          connection.primary_keys(tables).each do |table, primary_keys|
            primary_key = primary_keys.size > 1 ? primary_keys : primary_keys.first
            @primary_keys[deep_deduplicate(table)] = deep_deduplicate(primary_key)
          end

          connection.columns(tables).each do |table, columns|
            @columns[deep_deduplicate(table)] = deep_deduplicate(columns)
          end

          connection.indexes(tables).each do |table, indexes|
            @indexes[deep_deduplicate(table)] = deep_deduplicate(indexes)
          end

          tables.each do |table|
            add(pool, table)
          end

          version(pool)
        end
      end

      def dump_to(filename)
        open(filename) { |f|
          if filename.include?(".dump")
            f.write(Marshal.dump(self))
          elsif filename.include?(".json")
            f.write(JSONSchemaCacheSerializer.dump(self))
          else
            f.write(YAML.dump(self))
          end
        }
      end

      def marshal_dump # :nodoc:
        [@version, @columns, {}, @primary_keys, @data_sources, @indexes]
      end

      def marshal_load(array) # :nodoc:
        @version, @columns, _columns_hash, @primary_keys, @data_sources, @indexes, _database_version = array
        @indexes ||= {}
        @columns_hash = @columns.transform_values { |columns| columns.index_by(&:name) }
      end

      private
        def tables_to_cache(pool)
          pool.with_connection do |connection|
            connection.data_sources.reject do |table|
              ignored_table?(table)
            end
          end
        end

        def ignored_table?(table_name)
          ActiveRecord.schema_ignored_table?(table_name)
        end

        def derive_columns_hash_and_deduplicate_values
          @columns      = deep_deduplicate(@columns)
          @columns_hash = @columns.transform_values { |columns| columns.index_by(&:name) }
          @primary_keys = deep_deduplicate(@primary_keys)
          @data_sources = deep_deduplicate(@data_sources)
          @indexes      = deep_deduplicate(@indexes)
        end

        def deep_deduplicate(value)
          case value
          when Hash
            value.transform_keys { |k| deep_deduplicate(k) }.transform_values { |v| deep_deduplicate(v) }
          when Array
            value.map { |i| deep_deduplicate(i) }
          when String, Deduplicable
            -value
          else
            value
          end
        end

        def open(filename)
          FileUtils.mkdir_p(File.dirname(filename))

          File.atomic_write(filename) do |file|
            if File.extname(filename) == ".gz"
              zipper = Zlib::GzipWriter.new file
              zipper.mtime = 0
              yield zipper
              zipper.flush
              zipper.close
            else
              yield file
            end
          end
        end
    end
  end
end
