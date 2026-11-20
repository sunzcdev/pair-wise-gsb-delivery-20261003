# frozen_string_literal: true

require "cases/helper"
require "active_support/core_ext/object/with"

module ActiveRecord
  module ConnectionAdapters
    # Two SQLite databases that both define a table named +widgets+, but with
    # different columns and indexes. They share a single schema cache dump file
    # whose entries are keyed by the target database identity.
    class MultiSchemaCacheTest < ActiveRecord::TestCase
      self.use_transactional_tests = false

      TARGET_A = "primary"
      TARGET_B = "animals"
      TABLE = "widgets"

      class AppA < ActiveRecord::Base
        self.abstract_class = true
      end

      class AppB < ActiveRecord::Base
        self.abstract_class = true
      end

      class Builder < ActiveRecord::Base
        self.abstract_class = true
      end

      def setup
        @dir = Dir.mktmpdir
        @db_a = File.join(@dir, "db_a.sqlite3")
        @db_b = File.join(@dir, "db_b.sqlite3")
        @cache_path = File.join(@dir, "shared_schema_cache.yml")

        build_database(@db_a) do |conn|
          conn.create_table(TABLE) { |t| t.string :name; t.integer :count }
          conn.add_index(TABLE, :name, name: "idx_widgets_name_on_a")
        end

        build_database(@db_b) do |conn|
          conn.create_table(TABLE) { |t| t.string :title; t.decimal :price }
          conn.add_index(TABLE, :title, name: "idx_widgets_title_on_b")
        end

        AppA.establish_connection(shared_config(TARGET_A, @db_a))
        AppB.establish_connection(shared_config(TARGET_B, @db_b))
      end

      def teardown
        ActiveRecord::ConnectionAdapters::PoolConfig.discard_pools!
      ensure
        FileUtils.rm_rf(@dir)
      end

      def test_each_pool_reflection_is_bound_to_its_target_identity
        assert_equal TARGET_A, AppA.connection_pool.schema_reflection.instance_variable_get(:@target)
        assert_equal TARGET_B, AppB.connection_pool.schema_reflection.instance_variable_get(:@target)
      end

      def test_lazy_load_uses_only_the_pool_own_target_entry
        warm(AppA, TARGET_A)
        warm(AppB, TARGET_B)

        ActiveRecord.with(lazily_load_schema_cache: true) do
          AppA.remove_connection
          AppB.remove_connection
          AppA.establish_connection(shared_config(TARGET_A, @db_a))
          AppB.establish_connection(shared_config(TARGET_B, @db_b))

          AppA.connection_pool.lease_connection.verify!
          AppB.connection_pool.lease_connection.verify!

          cached_a = AppA.connection_pool.schema_reflection.instance_variable_get(:@cache)
          cached_b = AppB.connection_pool.schema_reflection.instance_variable_get(:@cache)

          assert_equal %w[id name count], cached_a.columns(AppA.connection_pool, TABLE).map(&:name)
          assert_equal %w[id title price], cached_b.columns(AppB.connection_pool, TABLE).map(&:name)
        end
      end

      def test_warming_each_database_writes_a_separate_entry_in_one_dump
        warm(AppA, TARGET_A)
        warm(AppB, TARGET_B)

        container = SchemaCache._load_from(@cache_path)
        assert_instance_of MultiSchemaCache, container
        assert_equal [TARGET_A, TARGET_B].sort, container.targets.sort
      end

      def test_second_warm_does_not_overwrite_the_first_database_entry
        warm(AppA, TARGET_A)
        warm(AppB, TARGET_B)

        loaded_a = SchemaCache._load_from(@cache_path)[TARGET_A]
        assert_equal %w[id name count], loaded_a.columns(AppA.connection_pool, TABLE).map(&:name)
        assert_equal ["idx_widgets_name_on_a"], loaded_a.indexes(AppA.connection_pool, TABLE).map(&:name)
      end

      def test_reflections_read_only_their_own_target_columns_and_indexes
        warm(AppA, TARGET_A)
        warm(AppB, TARGET_B)

        # Load in the reverse order from warming to prove no identity bleed.
        bound_b = load_reflection(TARGET_B, AppB.connection_pool)
        bound_a = load_reflection(TARGET_A, AppA.connection_pool)

        assert_equal %w[id title price], bound_b.columns(TABLE).map(&:name)
        assert_equal ["idx_widgets_title_on_b"], bound_b.indexes(TABLE).map(&:name)

        assert_equal %w[id name count], bound_a.columns(TABLE).map(&:name)
        assert_equal ["idx_widgets_name_on_a"], bound_a.indexes(TABLE).map(&:name)
      end

      def test_unknown_target_does_not_fall_back_to_another_database_entry
        warm(AppA, TARGET_A)
        warm(AppB, TARGET_B)

        reflection = SchemaReflection.new(@cache_path, target: "unrelated")
        bound = BoundSchemaReflection.new(reflection, AppA.connection_pool)
        bound.load!

        # Nothing on disk belongs to this target, so it behaves like an empty
        # cache rather than silently serving another database's columns.
        assert_not bound.cached?(TABLE)
      end

      def test_expired_version_only_invalidates_its_own_entry
        warm(AppA, TARGET_A)
        warm(AppB, TARGET_B)

        container = SchemaCache._load_from(@cache_path)
        stale = container[TARGET_B]
        stale.instance_variable_set(:@version, stale.schema_version + 100)
        container.dump_to(@cache_path)

        warning_b = capture(:stderr) do
          assert_nil load_cache(TARGET_B, AppB.connection_pool)
        end
        assert_match(/#{TARGET_B}.*expired/, warning_b)

        warning_a = capture(:stderr) do
          assert_instance_of SchemaCache, load_cache(TARGET_A, AppA.connection_pool)
        end
        assert_equal "", warning_a
      end

      def test_named_reflection_rejects_legacy_single_database_dump
        legacy_path = File.join(@dir, "legacy.yml")
        SchemaCache.new.tap { |c| c.add_all(AppA.connection_pool) }.dump_to(legacy_path)

        warning = capture(:stderr) do
          assert_nil load_cache_from(legacy_path, TARGET_A, AppA.connection_pool)
        end
        assert_match(/legacy single-database schema cache/, warning)
      end

      def test_unnamed_reflection_still_accepts_legacy_single_database_dump
        legacy_path = File.join(@dir, "legacy.yml")
        SchemaCache.new.tap { |c| c.add_all(AppA.connection_pool) }.dump_to(legacy_path)

        warning = capture(:stderr) do
          assert_instance_of SchemaCache, load_cache_from(legacy_path, nil, AppA.connection_pool)
        end
        assert_equal "", warning
      end

      def test_named_dump_upgrades_a_legacy_file_into_a_multi_dump
        legacy_path = File.join(@dir, "legacy.yml")
        SchemaCache.new.tap { |c| c.add_all(AppA.connection_pool) }.dump_to(legacy_path)

        SchemaReflection.new(legacy_path, target: TARGET_A).dump_to(AppA.connection_pool, legacy_path)

        reloaded = SchemaCache._load_from(legacy_path)
        assert_instance_of MultiSchemaCache, reloaded
        assert reloaded.key?(TARGET_A)
      end

      def test_multi_dump_round_trips_through_every_supported_format
        warm(AppA, TARGET_A)

        %w[yml dump json].each do |extension|
          path = File.join(@dir, "cache.#{extension}")
          warm(AppA, TARGET_A, path)

          container = SchemaCache._load_from(path)
          assert_instance_of MultiSchemaCache, container, "expected a container for .#{extension}"
          assert container.key?(TARGET_A), "expected the target entry for .#{extension}"
        end
      end

      def test_clearing_one_reflection_keeps_the_other_target_entry
        warm(AppA, TARGET_A)
        warm(AppB, TARGET_B)

        reflection_a = SchemaReflection.new(@cache_path, target: TARGET_A)
        BoundSchemaReflection.new(reflection_a, AppA.connection_pool).load!
        container = reflection_a.instance_variable_get(:@container)

        reflection_a.clear!

        # The cleared target now owns an empty in-memory entry, while the entry
        # belonging to the other database is left exactly as it was.
        assert_equal 0, container[TARGET_A].size
        assert container.key?(TARGET_B), "the other database entry must survive"
        assert_equal %w[id title price],
          container[TARGET_B].columns(AppB.connection_pool, TABLE).map(&:name)
      end

      def test_new_process_loading_in_reverse_order_reads_correct_columns_and_indexes
        warm(AppA, TARGET_A)
        warm(AppB, TARGET_B)

        load_paths = $LOAD_PATH.grep(/activerecord|activemodel|activesupport/)
          .select { |path| File.directory?(path) }.uniq

        script = <<~RUBY
          $LOAD_PATH.unshift(#{load_paths.map(&:inspect).join(", ")})
          require "json"
          require "active_record"

          def db_config(name, database, cache_path)
            ActiveRecord::DatabaseConfigurations::HashConfig.new(
              "test", name,
              adapter: "sqlite3",
              database: database,
              schema_cache_path: cache_path
            )
          end

          # Connect to B first, then A: the reverse of the warming order.
          class SubAppB < ActiveRecord::Base
            self.abstract_class = true
          end
          SubAppB.establish_connection(db_config(#{TARGET_B.inspect}, #{@db_b.inspect}, #{@cache_path.inspect}))
          b = SubAppB.connection_pool.schema_cache
          b_columns = b.columns(#{TABLE.inspect}).map(&:name)
          b_indexes = b.indexes(#{TABLE.inspect}).map(&:name)

          class SubAppA < ActiveRecord::Base
            self.abstract_class = true
          end
          SubAppA.establish_connection(db_config(#{TARGET_A.inspect}, #{@db_a.inspect}, #{@cache_path.inspect}))
          a = SubAppA.connection_pool.schema_cache
          a_columns = a.columns(#{TABLE.inspect}).map(&:name)
          a_indexes = a.indexes(#{TABLE.inspect}).map(&:name)

          puts JSON.pretty_generate(
            a_columns: a_columns, a_indexes: a_indexes,
            b_columns: b_columns, b_indexes: b_indexes
          )
        RUBY

        output = IO.popen([Gem.ruby, "-e", script], err: [:child, :out], &:read)
        assert_predicate $?, :success?, "subprocess failed:\n#{output}"

        result = JSON.parse(output)
        assert_equal %w[id name count], result.fetch("a_columns")
        assert_equal ["idx_widgets_name_on_a"], result.fetch("a_indexes")
        assert_equal %w[id title price], result.fetch("b_columns")
        assert_equal ["idx_widgets_title_on_b"], result.fetch("b_indexes")
      end

      private
        def build_database(path)
          Builder.establish_connection(adapter: "sqlite3", database: path)
          yield Builder.lease_connection
        ensure
          Builder.remove_connection
        end

        def shared_config(name, database)
          DatabaseConfigurations::HashConfig.new(
            "test", name,
            adapter: "sqlite3",
            database: database,
            schema_cache_path: @cache_path
          )
        end

        def warm(klass, target, path = @cache_path)
          pool = klass.connection_pool
          pool.schema_reflection.dump_to(pool, path)
          assert_equal target, pool.schema_reflection.instance_variable_get(:@target)
        end

        def load_reflection(target, pool)
          reflection = SchemaReflection.new(@cache_path, target: target)
          BoundSchemaReflection.new(reflection, pool).tap(&:load!)
        end

        def load_cache(target, pool)
          load_cache_from(@cache_path, target, pool)
        end

        def load_cache_from(path, target, pool)
          SchemaReflection.new(path, target: target).send(:load_cache, pool)
        end
    end
  end
end