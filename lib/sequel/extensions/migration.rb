# frozen-string-literal: true
#
# Adds the Sequel::Migration and Sequel::Migrator classes, which allow
# the user to easily group schema changes and migrate the database
# to a newer version or revert to a previous version.
#
# To load the extension:
#
#   Sequel.extension :migration
#
# Related modules: Sequel::Migration, Sequel::SimpleMigration,
# Sequel::MigrationDSL, Sequel::MigrationReverser, Sequel::MigrationAlterTableReverser,
# Sequel::Migrator, Sequel::IntegerMigrator, Sequel::TimestampMigrator

require 'digest'

#
module Sequel
  # Sequel's older migration class, available for backward compatibility.
  # Uses subclasses with up and down instance methods for each migration:
  # 
  #   Class.new(Sequel::Migration) do
  #     def up
  #       create_table(:artists) do
  #         primary_key :id
  #         String :name
  #       end
  #     end
  #     
  #     def down
  #       drop_table(:artists)
  #     end
  #   end
  #
  # Part of the +migration+ extension.
  class Migration
    # Set the database associated with this migration.
    def initialize(db)
      @db = db
    end
    
    # Applies the migration to the supplied database in the specified
    # direction.
    def self.apply(db, direction)
      raise(ArgumentError, "Invalid migration direction specified (#{direction.inspect})") unless [:up, :down].include?(direction)
      new(db).public_send(direction)
    end

    # Returns the list of Migration descendants.
    def self.descendants
      @descendants ||= []
    end
    
    # Adds the new migration class to the list of Migration descendants.
    def self.inherited(base)
      descendants << base
    end

    # Don't allow transaction overriding in old migrations.
    def self.use_transactions
      nil
    end
    
    # The default down action does nothing
    def down
    end
    
    # Intercepts method calls intended for the database and sends them along.
    def method_missing(method_sym, *args, &block)
      # Allow calling private methods for backwards compatibility
      @db.send(method_sym, *args, &block)
    end
    # simplecov:disable
    ruby2_keywords(:method_missing) if respond_to?(:ruby2_keywords, true)
    # simplecov:enable

    # This object responds to all methods the database responds to.
    def respond_to_missing?(meth, include_private)
      @db.respond_to?(meth, include_private)
    end

    # The default up action does nothing
    def up
    end
  end

  # Migration class used by the Sequel.migration DSL,
  # using instances for each migration, unlike the
  # +Migration+ class, which uses subclasses for each
  # migration. Part of the +migration+ extension.
  class SimpleMigration
    # Proc used for the down action
    attr_accessor :down

    # Proc used for the up action
    attr_accessor :up

    # Whether to use transactions for this migration, default depends on the
    # database.
    attr_accessor :use_transactions
    
    # Don't set transaction use by default.
    def initialize
      @use_transactions = nil
    end

    # Apply the appropriate block on the +Database+
    # instance using instance_exec.
    def apply(db, direction)
      raise(ArgumentError, "Invalid migration direction specified (#{direction.inspect})") unless [:up, :down].include?(direction)
      if prok = public_send(direction)
        db.instance_exec(&prok)
      end
    end
  end

  # Internal class used by the Sequel.migration DSL, part of the +migration+ extension.
  class MigrationDSL < BasicObject
    # The underlying SimpleMigration instance
    attr_reader :migration

    def self.create(&block)
      new(&block).migration
    end

    # Create a new migration class, and instance_exec the block.
    def initialize(&block)
      @migration = SimpleMigration.new
      Migration.descendants << migration
      instance_exec(&block)
    end

    # Defines the migration's down action.
    def down(&block)
      migration.down = block
    end

    # Disable the use of transactions for the related migration
    def no_transaction
      migration.use_transactions = false
    end

    # Enable the use of transactions for the related migration
    def transaction
      migration.use_transactions = true
    end

    # Defines the migration's up action.
    def up(&block)
      migration.up = block
    end

    # Creates a reversible migration.  This is the same as creating
    # the same block with +up+, but it also calls the block and attempts
    # to create a +down+ block that will reverse the changes made by
    # the block.
    #
    # There are no guarantees that this will work perfectly
    # in all cases, but it works for some simple cases.
    def change(&block)
      migration.up = block
      migration.down = MigrationReverser.new.reverse(&block)
    end

    # Creates a revert migration.  This is the same as creating
    # the same block with +down+, but it also calls the block and attempts
    # to create a +up+ block that will reverse the changes made by
    # the block.  This is designed to revert the changes in the
    # provided block.
    #
    # There are no guarantees that this will work perfectly
    # in all cases, but it works for some simple cases.
    def revert(&block)
      migration.down = block
      migration.up = MigrationReverser.new.reverse(&block)
    end
  end

  # Handles the reversing of reversible migrations.  Basically records
  # supported methods calls, translates them to reversed calls, and
  # returns them in reverse order.
  class MigrationReverser < Sequel::BasicObject
    def initialize
      @actions = []
    end

    # Reverse the actions for the given block.  Takes the block given
    # and returns a new block that reverses the actions taken by
    # the given block.
    def reverse(&block)
      instance_exec(&block)
    rescue NoMethodError => e
      Proc.new{raise Sequel::Error, "irreversible migration method \"#{e.name}\" used in #{block.source_location.first}, you may need to write your own down method"}
    rescue => e
      Proc.new{raise Sequel::Error, "unable to reverse migration due to #{e.class} in #{block.source_location.first}, you may need to write your own down method"}
    else
      actions = @actions.reverse
      Proc.new do
       actions.each do |a|
        pr = a.last.is_a?(Proc) ? a.pop : nil
        # Allow calling private methods as the reversing methods are private
        send(*a, &pr)
       end
      end
    end

    private

    def add_column(*args)
      @actions << [:drop_column, args[0], args[1]]
    end

    def add_index(*args)
      @actions << [:drop_index, *args]
    end

    def alter_table(table, &block)
      @actions << [:alter_table, table, MigrationAlterTableReverser.new.reverse(&block)]
    end

    def create_join_table(*args)
      @actions << [:drop_join_table, *args]
    end

    def create_table(name, opts=OPTS, &_)
      @actions << [:drop_table, name, opts]
    end

    def create_view(name, _, opts=OPTS)
      @actions << [:drop_view, name, opts]
    end

    def rename_column(table, name, new_name)
      @actions << [:rename_column, table, new_name, name]
    end

    def rename_table(table, new_name)
      @actions << [:rename_table, new_name, table]
    end
  end

  # Handles reversing an alter_table block in a reversible migration.
  class MigrationAlterTableReverser < Sequel::BasicObject
    def initialize
      @actions = []
    end

    def reverse(&block)
      instance_exec(&block)
      actions = @actions.reverse
      # Allow calling private methods as the reversing methods are private
      Proc.new{actions.each{|a| send(*a)}}
    end

    private

    def add_column(*args)
      @actions << [:drop_column, args.first]
    end

    def add_constraint(*args)
      name = args.first
      name = name.is_a?(Hash) ? name[:name] : name
      @actions << [:drop_constraint, name]
    end

    def add_foreign_key(key, table, *args)
      @actions << [:drop_foreign_key, key, *args]
    end

    def add_primary_key(*args)
      super if args.first.is_a?(Array)
      @actions << [:drop_column, args.first]
    end

    def add_index(*args)
      @actions << [:drop_index, *args]
    end
    alias add_full_text_index add_index
    alias add_spatial_index add_index

    def rename_column(name, new_name)
      @actions << [:rename_column, new_name, name]
    end

    def set_column_allow_null(name, allow_null=true)
      @actions << [:set_column_allow_null, name, !allow_null]
    end

    def set_column_not_null(name)
      @actions << [:set_column_allow_null, name]
    end
  end

  # The preferred method for writing Sequel migrations, using a DSL:
  # 
  #   Sequel.migration do
  #     up do
  #       create_table(:artists) do
  #         primary_key :id
  #         String :name
  #       end
  #     end
  #     
  #     down do
  #       drop_table(:artists)
  #     end
  #   end
  #
  # Designed to be used with the +Migrator+ class, part of the +migration+ extension.
  def self.migration(&block)
    MigrationDSL.create(&block)
  end

  # The +Migrator+ class performs migrations based on migration files in a 
  # specified directory. The migration files should be named using the
  # following pattern:
  # 
  #   <version>_<title>.rb
  #
  # For example, the following files are considered migration files:
  #   
  #   001_create_sessions.rb
  #   002_add_data_column.rb
  #   
  # You can also use timestamps as version numbers:
  #
  #   1273253850_create_sessions.rb
  #   1273257248_add_data_column.rb
  #
  # If any migration filenames use timestamps as version numbers, Sequel
  # uses the +TimestampMigrator+ to migrate, otherwise it uses the +IntegerMigrator+.
  # The +TimestampMigrator+ can handle migrations that are run out of order
  # as well as migrations with the same timestamp,
  # while the +IntegerMigrator+ is more strict and raises exceptions for missing
  # or duplicate migration files.
  #
  # The migration files should contain either one +Migration+
  # subclass or one <tt>Sequel.migration</tt> call.
  #
  # Migrations are generally run via the sequel command line tool,
  # using the -m and -M switches.  The -m switch specifies the migration
  # directory, and the -M switch specifies the version to which to migrate.
  # 
  # You can apply migrations using the Migrator API, as well (this is necessary
  # if you want to specify the version from which to migrate in addition to the version
  # to which to migrate).
  # To apply a migrator, the +apply+ method must be invoked with the database
  # instance, the directory of migration files and the target version. If
  # no current version is supplied, it is read from the database. The migrator
  # automatically creates a table (schema_info for integer migrations and
  # schema_migrations for timestamped migrations). in the database to keep track
  # of the current migration version. If no migration version is stored in the
  # database, the version is considered to be 0. If no target version is 
  # specified, or the target version specified is greater than the latest
  # version available, the database is migrated to the latest version available in the
  # migration directory.
  #
  # For example, to migrate the database to the latest version:
  #
  #   Sequel::Migrator.run(DB, '.')
  #
  # For example, to migrate the database all the way down:
  #
  #   Sequel::Migrator.run(DB, '.', target: 0)
  #
  # For example, to migrate the database to version 4:
  #
  #   Sequel::Migrator.run(DB, '.', target: 4)
  #
  # To migrate the database from version 1 to version 5:
  #
  #   Sequel::Migrator.run(DB, '.', target: 5, current: 1)
  #
  # By default only the migration number or filename is stored, so renaming
  # or modifying an already applied migration file goes undetected.  Passing
  # the <tt>migration_identities: true</tt> option additionally records the
  # filename, a full digest of the file contents, and the applied direction
  # for each successfully applied migration, and refuses to continue when an
  # applied migration file no longer matches its recorded identity.
  #
  # Part of the +migration+ extension.
  class Migrator
    MIGRATION_FILE_PATTERN = /\A(\d+)_(.+)\.rb\z/i.freeze

    # Mutex used around migration file loading
    MUTEX = Mutex.new

    # Exception class raised when there is an error with the migrator's
    # file structure, database, or arguments.
    class Error < Sequel::Error
    end

    # Exception class raised when Migrator.check_current signals that it is
    # not current.
    class NotCurrentError < Error
    end

    # Wrapper for +run+, maintaining backwards API compatibility
    def self.apply(db, directory, target = nil, current = nil)
      run(db, directory, :target => target, :current => current)
    end

    # Raise a NotCurrentError unless the migrator is current, takes the same
    # arguments as #run.
    def self.check_current(*args)
      raise(NotCurrentError, 'current migration version does not match latest available version') unless is_current?(*args)
    end

    # Return whether the migrator is current (i.e. it does not need to make
    # any changes).  Takes the same arguments as #run.
    def self.is_current?(db, directory, opts=OPTS)
      migrator_class(directory).new(db, directory, opts).is_current?
    end

    # Lock ID to use for advisory locks when running migrations
    # "sequel-migration".codepoints.reduce(:*) % (2**63)
    MIGRATION_ADVISORY_LOCK_ID = 4966325471869609408
    private_constant :MIGRATION_ADVISORY_LOCK_ID

    # Migrates the supplied database using the migration files in the specified directory. Options:
    # :allow_migration_number :: Set to check each migration number. If set, calls the
    #                            <tt>===</tt> method on the object with the migration number,
    #                            allowing it to work with both ranges and procs. If the
    #                            <tt>===</tt> method returns false, an exception is raised
    #                            (TimestampMigrator only).
    # :allow_missing_migration_files :: Don't raise an error if there are missing migration files.
    #                                   It is very risky to use this option, since it can result in
    #                                   the database schema version number not matching the expected
    #                                   database schema.
    # :column :: The column in the :table argument storing the migration version (default: :version).
    # :current :: The current version of the database.  If not given, it is retrieved from the database
    #             using the :table and :column options.
    # :migration_identities :: Record and verify the explicit content identity of each successfully
    #                          applied migration, in addition to the version or filename tracking.
    #                          For each migration the filename, a full digest of the file contents,
    #                          and the direction in which it was last applied (:up or :down) are
    #                          stored in a separate table.  Migrations whose recorded contents no
    #                          longer match the file on disk, or whose identity cannot be confirmed
    #                          because they were applied before identity tracking was enabled, are
    #                          refused instead of being assumed to be applied.
    # :migration_identity_table :: The table holding the explicit migration identity records
    #                              (default: :schema_migration_identities).  Only used when
    #                              :migration_identities is enabled.
    # :relative :: Run the given number of migrations, with a positive number being migrations to migrate
    #              up, and a negative number being migrations to migrate down (IntegerMigrator only).
    # :table :: The table containing the schema version (default: :schema_info for integer migrations and
    #           :schema_migrations for timestamped migrations).
    # :target :: The target version to which to migrate.  If not given, migrates to the maximum version.
    # :use_advisory_lock :: Use advisory locks in migrations (only use this if Sequel supports advisory
    #                       locks for the database).
    #
    # Examples: 
    #   Sequel::Migrator.run(DB, "migrations")
    #   Sequel::Migrator.run(DB, "migrations", target: 15, current: 10)
    #   Sequel::Migrator.run(DB, "app1/migrations", column: :app2_version)
    #   Sequel::Migrator.run(DB, "app2/migrations", column: :app2_version, table: :schema_info2)
    def self.run(db, directory, opts=OPTS)
      if opts[:use_advisory_lock]
        db.with_advisory_lock(MIGRATION_ADVISORY_LOCK_ID){run(db, directory, opts.merge(:use_advisory_lock=>false))}
      else
        migrator_class(directory).new(db, directory, opts).run
      end
    end

    # Choose the Migrator subclass to use.  Uses the TimestampMigrator
    # if the version number is greater than 20000101, otherwise uses the IntegerMigrator.
    def self.migrator_class(directory)
      if self.equal?(Migrator)
        raise(Error, "Must supply a valid migration path") unless File.directory?(directory)
        Dir.new(directory).each do |file|
          next unless MIGRATION_FILE_PATTERN.match(file)
          return TimestampMigrator if file.split('_', 2).first.to_i > 20000101
        end
        IntegerMigrator
      else
        self
      end
    end
    
    # The column to use to hold the migration version number for integer migrations or
    # filename for timestamp migrations (defaults to :version for integer migrations and
    # :filename for timestamp migrations)
    attr_reader :column

    # The database related to this migrator
    attr_reader :db

    # The directory for this migrator's files
    attr_reader :directory

    # The dataset for this migrator, representing the +schema_info+ table for integer
    # migrations and the +schema_migrations+ table for timestamp migrations
    attr_reader :ds

    # All migration files in this migrator's directory
    attr_reader :files

    # The table to use to hold the applied migration data (defaults to :schema_info for
    # integer migrations and :schema_migrations for timestamp migrations)
    attr_reader :table

    # The target version for this migrator
    attr_reader :target

    # Whether explicit migration identity records are kept and verified.
    # When true, every successfully applied migration records its filename,
    # a full digest of its contents, and the direction in which it was applied.
    attr_reader :migration_identities

    # The table used to store explicit migration identity records, when
    # migration identity tracking is enabled.
    attr_reader :migration_identity_table

    # Setup the state for the migrator
    def initialize(db, directory, opts=OPTS)
      raise(Error, "Must supply a valid migration path") unless File.directory?(directory)
      @db = db
      @directory = directory
      @allow_missing_migration_files = opts[:allow_missing_migration_files]
      @files = get_migration_files
      schema, table = @db.send(:schema_and_table, opts[:table] || default_schema_table)
      @table = schema ? Sequel::SQL::QualifiedIdentifier.new(schema, table) : table
      @column = opts[:column] || default_schema_column
      @ds = schema_dataset
      @use_transactions = opts[:use_transactions]
      @migration_identities = opts[:migration_identities] ? true : false
      @migration_identity_table = opts[:migration_identity_table] || :schema_migration_identities
      @unverified_migrations = []
      if migration_identities
        schema, id_table = @db.send(:schema_and_table, @migration_identity_table)
        @migration_identity_table = schema ? Sequel::SQL::QualifiedIdentifier.new(schema, id_table) : id_table
        @migration_identity_ds = migration_identity_dataset
        @migration_file_digests = {}
        @recorded_migration_identities = get_recorded_migration_identities
      end
    end

    # Whether the content identity of the given migration file has been
    # confirmed against an explicit identity record.  Returns false when
    # identity tracking is disabled, or when the migration was applied
    # before identity records existed (in which case it is listed in
    # #unverified_migrations and its identity must not be treated as verified).
    def migration_identity_verified?(file_name)
      return false unless migration_identities
      fi = file_name.downcase
      record = recorded_migration_identities[fi]
      return false unless record
      current = current_migration_identities[fi]
      !!current && record[migration_identity_digest_column] == current[migration_identity_digest_column]
    end

    # Return a frozen copy of the explicit identity record stored for the
    # given migration filename, or nil if no identity record exists.  A
    # record only exists for migrations successfully applied while identity
    # tracking was enabled; legacy migrations have no record and therefore
    # no verifiable identity.
    def migration_identity_for(file_name)
      return nil unless migration_identities
      record = recorded_migration_identities[file_name.downcase]
      record && record.dup.freeze
    end

    # Filenames of migrations that are treated as applied but whose content
    # identity cannot be confirmed, because they were applied before identity
    # tracking was enabled.  These migrations are never marked as verified and
    # no identity record is fabricated for them; applying them in a direction
    # that would change the schema raises a Migrator::Error.
    attr_reader :unverified_migrations

    private

    # If transactions should be used for the migration, yield to the block
    # inside a transaction.  Otherwise, just yield to the block.
    def checked_transaction(migration, &block)
      use_trans = if @use_transactions.nil?
        if migration.use_transactions.nil?
          @db.supports_transactional_ddl?
        else
          migration.use_transactions
        end
      else
        @use_transactions
      end

      db.transaction(:skip_transaction=>use_trans == false, &block)
    end

    # Load the migration file, raising an exception if the file does not define
    # a single migration.
    def load_migration_file(file)
      MUTEX.synchronize do
        n = Migration.descendants.length
        load(file)
        raise Error, "Migration file #{file.inspect} not containing a single migration detected" unless n + 1 == Migration.descendants.length
        c = Migration.descendants.pop
        if c.is_a?(Class) && !c.name.to_s.empty? && Object.const_defined?(c.name)
          Object.send(:remove_const, c.name)
        end
        c
      end
    end

    # Return the integer migration version based on the filename.
    def migration_version_from_file(filename)
      filename.split('_', 2).first.to_i
    end

    # The digest column used in the migration identity table.
    def migration_identity_digest_column
      :digest
    end

    # The direction column used in the migration identity table.
    def migration_identity_direction_column
      :direction
    end

    # The filename column used in the migration identity table.
    def migration_identity_filename_column
      :filename
    end

    # Return the dataset for the migration identity table, creating the
    # table if it does not exist.  One row is kept per migration filename,
    # holding the full digest of the file contents when it was last applied
    # and the direction in which it was applied.
    def migration_identity_dataset
      c = migration_identity_filename_column
      d = migration_identity_digest_column
      r = migration_identity_direction_column
      db.create_table?(migration_identity_table) do
        String c, :primary_key=>true
        String d, :null=>false
        String r, :null=>false
      end
      db.from(migration_identity_table)
    end

    # Compute the full content digest of the migration file.  The file is
    # read every time, so a change made while the migrator is running is
    # detected rather than masked by a cached value.
    def migration_file_digest(path)
      @migration_file_digests[path] ||= Digest::SHA256.file(path).hexdigest
    end

    # Map of downcased migration filename to the identity (digest and
    # direction) computed from the files currently on disk.
    def current_migration_identities
      h = {}
      files.compact.each do |path|
        file_name = File.basename(path)
        fi = file_name.downcase
        raise(Error, "Duplicate migration filename identity: #{file_name}") if h.key?(fi)
        h[fi] = {
          migration_identity_digest_column => migration_file_digest(path),
          migration_identity_direction_column => nil
        }
      end
      h
    end

    # Map of downcased migration filename to the identity record stored in
    # the database for migrations applied with identity tracking enabled.
    def get_recorded_migration_identities
      h = {}
      fc = migration_identity_filename_column
      dc = migration_identity_digest_column
      rc = migration_identity_direction_column
      @migration_identity_ds.each do |row|
        fi = row[fc].downcase
        raise(Error, "Duplicate migration identity record: #{row[fc]}") if h.key?(fi)
        h[fi] = {dc => row[dc], rc => row[rc]}
      end
      h
    end

    attr_reader :recorded_migration_identities

    # Compare the identity records stored in the database against the
    # migration files currently on disk and the migrations treated as
    # applied.  Any mismatch is a safety violation and raises an error;
    # applied migrations without an identity record are reported as
    # unverifiable instead of being silently treated as verified.
    def verify_migration_identities
      current = current_migration_identities
      recorded = recorded_migration_identities
      dc = migration_identity_digest_column

      recorded.each do |fi, record|
        next unless current.key?(fi)
        if record[dc] != current[fi][dc]
          raise(Error, "Migration file has been modified since it was applied: #{fi}")
        end
      end

      @unverified_migrations = applied_migration_filenames.select do |fi|
        !recorded.key?(fi)
      end.freeze
    end

    # Downcased filenames of migrations the migrator currently treats as
    # applied based on the ordinary version/filename tracking.
    def applied_migration_filenames
      files.compact.map{|f| File.basename(f).downcase}.select do |fi|
        migration_file_applied?(fi)
      end
    end

    # Whether the migration with the given downcased filename is treated as
    # applied by the ordinary version/filename tracking.
    def migration_file_applied?(fi)
      raise NotImplementedError, "#{self.class} should implement #migration_file_applied?"
    end

    # Raise unless the identity of the migration about to be applied can be
    # confirmed.  New migrations may be applied without an existing record,
    # but re-applying or reverting a legacy migration whose contents were
    # never recorded is refused, since there is no way to know what was
    # actually run in the past.  The current digest is always computed and
    # cached before the migration runs, so a file changed while it is being
    # executed can be detected afterwards in #record_migration_identity.
    def check_migration_identity_before_apply(file_name, direction)
      fi = file_name.downcase
      if unverified_migrations.include?(fi)
        raise(Error, "Cannot apply migration #{direction} for #{fi}, its content identity cannot be confirmed because it was applied before migration identity tracking was enabled")
      end
      path = files.compact.find{|f| File.basename(f).downcase == fi}
      digest = migration_file_digest(path)
      if (record = recorded_migration_identities[fi]) && record[migration_identity_digest_column] != digest
        raise(Error, "Migration file has been modified since it was applied: #{fi}")
      end
    end

    # Persist the explicit identity record for a successfully applied
    # migration.  The file is re-read inside the migration transaction and
    # compared to the digest computed before the migration ran, so a script
    # changed during execution is detected and the whole migration (including
    # its identity record and version update) is rolled back.
    def record_migration_identity(file_name, path, direction)
      fi = file_name.downcase
      expected_digest = migration_file_digest(path)
      digest = Digest::SHA256.file(path).hexdigest
      if digest != expected_digest
        raise(Error, "Migration file changed while it was being applied: #{fi}")
      end
      @migration_file_digests[path] = digest

      fc = migration_identity_filename_column
      dc = migration_identity_digest_column
      rc = migration_identity_direction_column
      if recorded_migration_identities.key?(fi)
        @migration_identity_ds.where(fc=>fi).update(dc=>digest, rc=>direction.to_s)
      else
        @migration_identity_ds.insert(fc=>fi, dc=>digest, rc=>direction.to_s)
      end
      recorded_migration_identities[fi] = {dc=>digest, rc=>direction.to_s}
    end
  end

  # The default migrator, recommended in most cases.  Uses a simple incrementing
  # version number starting with 1, where missing or duplicate migration file
  # versions are not allowed.  Part of the +migration+ extension.
  class IntegerMigrator < Migrator
    Error = Migrator::Error

    # The current version for this migrator
    attr_reader :current

    # The direction of the migrator, either :up or :down
    attr_reader :direction

    # The migrations used by this migrator
    attr_reader :migrations

    # Set up all state for the migrator instance
    def initialize(db, directory, opts=OPTS)
      super
      @current = opts[:current] || current_migration_version

      latest_version = latest_migration_version
      @target = if opts[:target]
        opts[:target]
      elsif opts[:relative]
        @current + opts[:relative]
      else
        latest_version
      end

      raise(Error, "No target and/or latest version available, probably because no migration files found or filenames don't follow the migration filename convention") unless target && latest_version

      if @target > latest_version
        @target = latest_version
      elsif @target < 0
        @target = 0
      end

      @direction = current < target ? :up : :down

      if @direction == :down && @current >= @files.length && !@allow_missing_migration_files
        raise Migrator::Error, "Missing migration version(s) needed to migrate down to target version (current: #{current}, target: #{target})"
      end

      @migrations = get_migrations
      verify_migration_identities if migration_identities
    end

    # The integer migrator is current if the current version is the same as the target version.
    def is_current?
      current_migration_version == target
    end

    # Apply all migrations on the database
    def run
      migrations.zip(version_numbers).each do |m, v|
        timer = Sequel.start_timer
        path = files[v]
        file_name = File.basename(path)
        db.log_info("Begin applying migration version #{v}, direction: #{direction}")
        checked_transaction(m) do
          check_migration_identity_before_apply(file_name, direction) if migration_identities
          m.apply(db, direction)
          set_migration_version(up? ? v : v-1)
          record_migration_identity(file_name, path, direction) if migration_identities
        end
        db.log_info("Finished applying migration version #{v}, direction: #{direction}, took #{sprintf('%0.6f', Sequel.elapsed_seconds_since(timer))} seconds")
      end

      target
    end

    private

    # Whether the migration with the given downcased filename is treated as
    # applied according to the stored integer version.
    def migration_file_applied?(fi)
      migration_version_from_file(fi) <= current
    end

    # Gets the current migration version stored in the database. If no version
    # number is stored, 0 is returned.
    def current_migration_version
      ds.get(column) || 0
    end

    # The default column storing schema version.
    def default_schema_column
      :version
    end

    # The default table storing schema version.
    def default_schema_table
      :schema_info
    end

    # Returns any found migration files in the supplied directory.
    def get_migration_files
      files = []
      Dir.new(directory).each do |file|
        next unless MIGRATION_FILE_PATTERN.match(file)
        version = migration_version_from_file(file)
        if version >= 20000101
          raise Migrator::Error, "Migration number too large, must use TimestampMigrator: #{file}"
        end
        raise(Error, "Duplicate migration version: #{version}") if files[version]
        files[version] = File.join(directory, file)
      end
      1.upto(files.length - 1){|i| raise(Error, "Missing migration version: #{i}") unless files[i]} unless @allow_missing_migration_files
      files
    end
    
    # Returns a list of migration classes filtered for the migration range and
    # ordered according to the migration direction.
    def get_migrations
      version_numbers.map{|n| load_migration_file(files[n])}
    end
    
    # Returns the latest version available in the specified directory.
    def latest_migration_version
      l = files.last
      l ? migration_version_from_file(File.basename(l)) : nil
    end
    
    # Returns the dataset for the schema_info table. If no such table
    # exists, it is automatically created.
    def schema_dataset
      c = column
      ds = db.from(table)
      db.create_table?(table){Integer c, :default=>0, :null=>false}
      unless ds.columns.include?(c)
        db.alter_table(table){add_column c, Integer, :default=>0, :null=>false}
      end
      ds.insert(c=>0) if ds.empty?
      raise(Error, "More than 1 row in migrator table") if ds.count > 1
      ds
    end
    
    # Sets the current migration version stored in the database.
    def set_migration_version(version)
      ds.update(column=>version)
    end

    # Whether or not this is an up migration
    def up?
      direction == :up
    end

    # An array of numbers corresponding to the migrations,
    # so that each number in the array is the migration version
    # that will be in affect after the migration is run.
    def version_numbers
      @version_numbers ||= begin
        versions = files.
          compact.
          map{|f| migration_version_from_file(File.basename(f))}.
          select{|v| up? ? (v > current && v <= target) : (v <= current && v > target)}.
          sort
        versions.reverse! unless up?
        versions
      end
    end
  end

  # The migrator used if any migration file version is greater than 20000101.
  # Stores filenames of migration files, and can figure out which migrations
  # have not been applied and apply them, even if earlier migrations are added
  # after later migrations.  If you plan to do that, the responsibility is on
  # you to make sure the migrations don't conflict. Part of the +migration+ extension.
  class TimestampMigrator < Migrator
    Error = Migrator::Error

    # Array of strings of applied migration filenames
    attr_reader :applied_migrations

    # Get tuples of migrations, filenames, and actions for each migration
    attr_reader :migration_tuples

    # Set up all state for the migrator instance
    def initialize(db, directory, opts=OPTS)
      @allow_migration_number = opts[:allow_migration_number]
      super
      @target = opts[:target]
      @applied_migrations = get_applied_migrations
      @migration_tuples = get_migration_tuples
      verify_migration_identities if migration_identities
    end

    # Apply the migration in the given file path.  See Migrator.run for the
    # available options.  Additionally, this method supports the :direction
    # option for whether to run the migration up (default) or down.
    def self.run_single(db, path, opts=OPTS)
      new(db, File.dirname(path), opts).run_single(path, opts[:direction] || :up)
    end

    # The timestamp migrator is current if there are no migrations to apply
    # in either direction.
    def is_current?
      migration_tuples.empty?
    end
    
    # Apply all migration tuples on the database
    def run
      migration_tuples.each do |m, f, direction|
        apply_migration(m, f, direction)
      end
      nil
    end

    # Apply single migration tuple at the given path with the given direction
    # on the database.
    def run_single(path, direction)
      migration = load_migration_file(path)
      file_name = File.basename(path)
      already_applied = applied_migrations.include?(file_name.downcase)

      return if direction == :up ? already_applied : !already_applied

      apply_migration(migration, file_name, direction, path)
      nil
    end

    private

    # Apply a single migration with the given filename in the given direction.
    def apply_migration(migration, file_name, direction, path=nil)
      fi = file_name.downcase
      t = Time.now

      path ||= files.find{|f| File.basename(f).downcase == fi}
      db.log_info("Begin applying migration #{file_name}, direction: #{direction}")
      checked_transaction(migration) do
        check_migration_identity_before_apply(file_name, direction) if migration_identities
        migration.apply(db, direction)
        direction == :up ? ds.insert(column=>fi) : ds.where(column=>fi).delete
        record_migration_identity(file_name, path, direction) if migration_identities
      end
      db.log_info("Finished applying migration #{file_name}, direction: #{direction}, took #{sprintf('%0.6f', Time.now - t)} seconds")
    end

    # Convert the schema_info table to the new schema_migrations table format,
    # using the version of the schema_info table and the current migration files.
    def convert_from_schema_info
      v = db[:schema_info].get(:version)
      ds = db.from(table)
      files.each do |path|
        f = File.basename(path)
        if migration_version_from_file(f) <= v
          ds.insert(column=>f)
        end
      end
    end

    # The default column storing migration filenames.
    def default_schema_column
      :filename
    end

    # The default table storing migration filenames.
    def default_schema_table
      :schema_migrations
    end

    # Returns filenames of all applied migrations
    def get_applied_migrations
      am = ds.select_order_map(column)
      missing_migration_files = am - files.map{|f| File.basename(f).downcase}
      raise(Error, "Applied migration files not in file system: #{missing_migration_files.join(', ')}") if missing_migration_files.length > 0 && !@allow_missing_migration_files
      am
    end

    # Whether the migration with the given downcased filename is treated as
    # applied according to the stored filenames.
    def migration_file_applied?(fi)
      applied_migrations.include?(fi)
    end
    
    # Returns any migration files found in the migrator's directory.
    def get_migration_files
      files = []
      allow_migration_number = @allow_migration_number

      Dir.new(directory).each do |file|
        next unless MIGRATION_FILE_PATTERN.match(file)
        files << File.join(directory, file)
      end

      files.map! do |f|
        ver, = split = split_migration_filename(f)
        if allow_migration_number && !(allow_migration_number === ver)
          raise Error, "Migration filename uses version number that is not allowed: #{f}"
        end
        [f, *split]
      end.sort! do |a, b|
        _, a_ver, a_name = a
        _, b_ver, b_name = b
        x = a_ver <=> b_ver
        if x.zero?
          x = a_name <=> b_name
        end
        x
      end.map!(&:first)
    end

    # Return an integer and name (without extension) for the given path.
    def split_migration_filename(path)
      version, name = MIGRATION_FILE_PATTERN.match(File.basename(path)).captures
      version = version.to_i
      [version, name]
    end
    
    # Returns tuples of migration, filename, and direction
    def get_migration_tuples
      up_mts = []
      down_mts = []
      files.each do |path|
        f = File.basename(path)
        fi = f.downcase
        if target
          if migration_version_from_file(f) > target
            if applied_migrations.include?(fi)
              down_mts << [load_migration_file(path), f, :down]
            end
          elsif !applied_migrations.include?(fi)
            up_mts << [load_migration_file(path), f, :up]
          end
        elsif !applied_migrations.include?(fi)
          up_mts << [load_migration_file(path), f, :up]
        end
      end
      up_mts + down_mts.reverse
    end
    
    # Returns the dataset for the schema_migrations table. If no such table
    # exists, it is automatically created.
    def schema_dataset
      c = column
      ds = db.from(table)
      if !db.table_exists?(table)
        begin
          db.create_table(table){String c, :primary_key=>true}
        rescue Sequel::DatabaseError => e
          if db.database_type == :mysql && e.message.include?('max key length')
            # Handle case where MySQL is used with utf8mb4 charset default, which
            # only allows a maximum length of about 190 characters for string
            # primary keys due to InnoDB limitations.
            db.create_table(table){String c, :primary_key=>true, :size=>190}
          else
            raise e
          end
        end
        if db.table_exists?(:schema_info) and vha = db[:schema_info].all and vha.length == 1 and
           vha.first.keys == [:version] and vha.first.values.first.is_a?(Integer)
          convert_from_schema_info
        end
      elsif !ds.columns.include?(c)
        raise(Error, "Migrator table #{table} does not contain column #{c}")
      end
      ds
    end
  end
end
