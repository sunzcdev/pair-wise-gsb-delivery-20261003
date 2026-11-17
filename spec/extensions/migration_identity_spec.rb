# frozen_string_literal: true
require_relative "spec_helper"

Sequel.extension :migration

describe "Sequel migration identity tracking" do
  before do
    @tmpdirs = []
    @databases = []
  end

  after do
    @databases.each do |db|
      db.disconnect rescue nil
    end
    GC.start
    @tmpdirs.each do |dir|
      3.times do
        FileUtils.rm_rf(dir)
        break unless File.directory?(dir)
        sleep 0.05
      end
    end
    # Remove any database files created directly in the temporary directory.
    @database_paths.to_a.each do |path|
      3.times do
        FileUtils.rm_f(path)
        break unless File.file?(path)
        sleep 0.05
      end
    end
  end

  def sqlite_path
    path = File.join(Dir.tmpdir, "sequel_migration_identity_#{Process.pid}_#{object_id}_#{@databases.length}.db")
    (@database_paths ||= []) << path
    path
  end

  def migration_dir
    dir = File.join(Dir.tmpdir, "sequel_migration_identity_#{Process.pid}_#{object_id}_#{@tmpdirs.length}")
    FileUtils.mkdir_p(dir)
    @tmpdirs << dir
    dir
  end

  def sqlite_database(path = sqlite_path)
    db = Sequel.sqlite(path)
    @databases << db
    db
  end

  def write_migration(dir, filename, table, extra = "")
    File.write(File.join(dir, filename), <<~RUBY)
      Sequel.migration do
        up do
          create_table(:#{table}){Integer :id}
          #{extra}
        end
        down do
          drop_table(:#{table})
        end
      end
    RUBY
    File.join(dir, filename)
  end

  def digest(path)
    Digest::SHA256.file(path).hexdigest
  end

  def identity_rows(db, table = :schema_migration_identities)
    db.from(table).order(:filename).all
  end

  # Shared examples run by both migrator describe blocks.  The including
  # describe block must define #migrator_class, #version_prefix,
  # #first_file_name, #second_file_name, #renamed_file_name,
  # #rollback_target and #applied_version.
  shared_behavior = proc do
    it "records the filename, full content digest and direction for each successfully applied migration" do
      dir = migration_dir
      first = write_migration(dir, first_file_name, :first_table)
      second = write_migration(dir, second_file_name, :second_table)
      db = sqlite_database

      Sequel::Migrator.run(db, dir, :migration_identities=>true)

      identity_rows(db).must_equal [
        {:filename=>first_file_name.downcase, :digest=>digest(first), :direction=>"up"},
        {:filename=>second_file_name.downcase, :digest=>digest(second), :direction=>"up"}
      ]
      db.table_exists?(:first_table).must_equal true
      db.table_exists?(:second_table).must_equal true
    end

    it "does not fabricate identity records when tracking is disabled" do
      dir = migration_dir
      write_migration(dir, first_file_name, :first_table)
      db = sqlite_database

      Sequel::Migrator.run(db, dir)
      db.table_exists?(:schema_migration_identities).must_equal false
    end

    it "treats previously applied migrations as unverifiable when identity records do not exist" do
      dir = migration_dir
      write_migration(dir, first_file_name, :first_table)
      db = sqlite_database

      Sequel::Migrator.run(db, dir)

      migrator = migrator_class.new(db, dir, :migration_identities=>true)
      migrator.migration_identities.must_equal true
      migrator.unverified_migrations.must_equal [first_file_name.downcase]
      migrator.migration_identity_verified?(first_file_name).must_equal false
      migrator.migration_identity_for(first_file_name).must_be_nil
      identity_rows(db).must_equal []
    end

    it "confirms identity when reopening the database without changes" do
      dir = migration_dir
      first = write_migration(dir, first_file_name, :first_table)
      path = sqlite_path
      db = sqlite_database(path)

      Sequel::Migrator.run(db, dir, :migration_identities=>true)
      db.disconnect

      db = sqlite_database(path)

      migrator = migrator_class.new(db, dir, :migration_identities=>true)
      migrator.unverified_migrations.must_equal []
      migrator.migration_identity_verified?(first_file_name).must_equal true
      identity = migrator.migration_identity_for(first_file_name)
      identity[:digest].must_equal digest(first)
      identity[:direction].must_equal "up"
      identity.frozen?.must_equal true
    end

    it "refuses to continue when an applied migration file has modified contents" do
      dir = migration_dir
      write_migration(dir, first_file_name, :first_table)
      second = write_migration(dir, second_file_name, :second_table)
      db = sqlite_database

      Sequel::Migrator.run(db, dir, :migration_identities=>true)

      File.write(second, File.read(second) + "\n# silently modified after being applied\n")

      error = proc{migrator_class.new(db, dir, :migration_identities=>true)}.must_raise Sequel::Migrator::Error
      error.message.must_include second_file_name.downcase
      error.message.must_include "modified"
    end

    it "refuses to continue when an applied migration file has been removed or renamed" do
      dir = migration_dir
      write_migration(dir, first_file_name, :first_table)
      write_migration(dir, second_file_name, :second_table)
      db = sqlite_database

      Sequel::Migrator.run(db, dir, :migration_identities=>true)

      if Sequel::IntegerMigrator.equal?(migrator_class)
        # IntegerMigrator requires consecutive versions, so removing the last
        # file keeps the remaining version sequence valid while leaving an
        # applied migration without a file on disk.
        FileUtils.rm(File.join(dir, second_file_name))
      else
        FileUtils.mv(File.join(dir, first_file_name), File.join(dir, renamed_file_name))
      end

      proc{migrator_class.new(db, dir, :migration_identities=>true)}.must_raise Sequel::Migrator::Error
    end

    it "records the down direction when rolling back applied migrations" do
      dir = migration_dir
      first = write_migration(dir, first_file_name, :first_table)
      write_migration(dir, second_file_name, :second_table)
      db = sqlite_database

      Sequel::Migrator.run(db, dir, :migration_identities=>true)
      rollback_target(db, dir)

      rows = identity_rows(db)
      rows.map{|r| r[:filename]}.must_equal [first_file_name.downcase, second_file_name.downcase]
      rows.each{|r| r[:direction].must_equal "down"}
      rows.find{|r| r[:filename] == first_file_name.downcase}[:digest].must_equal digest(first)
    end

    it "can migrate forward again after rolling back, updating the recorded direction" do
      dir = migration_dir
      write_migration(dir, first_file_name, :first_table)
      db = sqlite_database

      Sequel::Migrator.run(db, dir, :migration_identities=>true)
      rollback_target(db, dir)
      identity_rows(db).first[:direction].must_equal "down"

      Sequel::Migrator.run(db, dir, :migration_identities=>true)
      identity_rows(db).first[:direction].must_equal "up"
      db.table_exists?(:first_table).must_equal true
    end

    it "refuses to roll back legacy migrations whose identity cannot be confirmed" do
      dir = migration_dir
      write_migration(dir, first_file_name, :first_table)
      write_migration(dir, second_file_name, :second_table)
      db = sqlite_database

      Sequel::Migrator.run(db, dir)

      error = nil
      begin
        rollback_target(db, dir, :migration_identities=>true)
      rescue Sequel::Migrator::Error => e
        error = e
      end
      error.wont_be_nil
      error.message.must_include "content identity cannot be confirmed"
      applied_version(db).must_equal 2
      db.table_exists?(:second_table).must_equal true
    end

    it "does not register success when a migration transaction fails" do
      dir = migration_dir
      write_migration(dir, first_file_name, :first_table)
      bad = File.join(dir, second_file_name)
      File.write(bad, <<~RUBY)
        Sequel.migration do
          up do
            create_table(:second_table){Integer :id}
            get(:table_that_does_not_exist)
          end
        end
      RUBY
      db = sqlite_database

      proc{Sequel::Migrator.run(db, dir, :migration_identities=>true)}.must_raise Sequel::DatabaseError

      applied_version(db).must_equal 1
      identity_rows(db).map{|r| r[:filename]}.must_equal [first_file_name.downcase]
      db.table_exists?(:schema_migration_identities).must_equal true
    end

    it "rejects a migration script that changes contents while it is being applied" do
      dir = migration_dir
      path = File.join(dir, first_file_name)
      File.write(path, <<~'RUBY')
        Sequel.migration do
          up do
            create_table(:first_table){Integer :id}
            File.write(__FILE__, File.read(__FILE__) + "\n# changed during execution\n")
          end
          down do
            drop_table(:first_table)
          end
        end
      RUBY
      db = sqlite_database

      error = proc{Sequel::Migrator.run(db, dir, :migration_identities=>true)}.must_raise Sequel::Migrator::Error
      error.message.must_include "changed while it was being applied"
      identity_rows(db).must_equal []
    end

    it "supports a custom identity table name" do
      dir = migration_dir
      write_migration(dir, first_file_name, :first_table)
      db = sqlite_database

      Sequel::Migrator.run(db, dir, :migration_identities=>true, :migration_identity_table=>:migration_audit)

      db.table_exists?(:migration_audit).must_equal true
      db.from(:migration_audit).count.must_equal 1
      migrator = migrator_class.new(db, dir, :migration_identities=>true, :migration_identity_table=>:migration_audit)
      migrator.migration_identity_table.to_s.must_equal "migration_audit"
      migrator.migration_identity_verified?(first_file_name).must_equal true
    end
  end

  describe "with IntegerMigrator" do
    instance_exec(&shared_behavior)

    def version_prefix(n)
      "%03d" % n
    end

    def first_file_name
      "#{version_prefix(1)}_create_first.rb"
    end

    def second_file_name
      "#{version_prefix(2)}_create_second.rb"
    end

    def renamed_file_name
      "001_create_renamed.rb"
    end

    def migrator_class
      Sequel::IntegerMigrator
    end

    def rollback_target(db, dir, opts = {})
      Sequel::Migrator.run(db, dir, opts.merge(:target=>0, :migration_identities=>true))
    end

    def applied_version(db)
      db[:schema_info].get(:version)
    end

    it "refuses duplicate migration version numbers" do
      dir = migration_dir
      write_migration(dir, "001_first.rb", :first_table)
      write_migration(dir, "001_second.rb", :second_table)
      db = sqlite_database

      error = proc{Sequel::Migrator.run(db, dir, :migration_identities=>true)}.must_raise Sequel::Migrator::Error
      error.message.must_include "Duplicate migration version"
    end

    it "keeps the ordinary schema_info version bookkeeping working alongside identity records" do
      dir = migration_dir
      write_migration(dir, first_file_name, :first_table)
      write_migration(dir, second_file_name, :second_table)
      db = sqlite_database

      Sequel::Migrator.run(db, dir, :migration_identities=>true)
      applied_version(db).must_equal 2
      Sequel::Migrator.run(db, dir, :target=>1, :migration_identities=>true)
      applied_version(db).must_equal 1
    end
  end

  describe "with TimestampMigrator" do
    instance_exec(&shared_behavior)

    def version_prefix(n)
      (1273253848 + n).to_s
    end

    def first_file_name
      "#{version_prefix(1)}_create_first.rb"
    end

    def second_file_name
      "#{version_prefix(2)}_create_second.rb"
    end

    def renamed_file_name
      "1273253849_create_renamed.rb"
    end

    def migrator_class
      Sequel::TimestampMigrator
    end

    def rollback_target(db, dir, opts = {})
      Sequel::Migrator.run(db, dir, opts.merge(:target=>0, :migration_identities=>true))
    end

    def applied_version(db)
      db[:schema_migrations].count
    end

    it "refuses same filename with different contents used from another directory" do
      dir_a = migration_dir
      dir_b = migration_dir
      write_migration(dir_a, first_file_name, :first_table)
      other = File.join(dir_b, first_file_name)
      File.write(other, <<~RUBY)
        Sequel.migration do
          up do
            create_table(:first_table){String :different_content}
          end
          down do
            drop_table(:first_table)
          end
        end
      RUBY
      db = sqlite_database

      Sequel::TimestampMigrator.run(db, dir_a, :migration_identities=>true)
      error = proc{Sequel::TimestampMigrator.run(db, dir_b, :target=>0, :migration_identities=>true)}.
        must_raise Sequel::Migrator::Error
      error.message.must_include "modified"
    end

    it "verifies matching contents for the same migration filename from another directory" do
      dir_a = migration_dir
      dir_b = migration_dir
      path_a = write_migration(dir_a, first_file_name, :first_table)
      File.write(File.join(dir_b, first_file_name), File.read(path_a))
      db = sqlite_database

      Sequel::TimestampMigrator.run(db, dir_a, :migration_identities=>true)
      migrator = Sequel::TimestampMigrator.new(db, dir_b, :migration_identities=>true)
      migrator.migration_identity_verified?(first_file_name).must_equal true
      migrator.unverified_migrations.must_equal []
    end

    it "keeps the ordinary schema_migrations filename bookkeeping working alongside identity records" do
      dir = migration_dir
      write_migration(dir, first_file_name, :first_table)
      write_migration(dir, second_file_name, :second_table)
      db = sqlite_database

      Sequel::Migrator.run(db, dir, :migration_identities=>true)
      applied_version(db).must_equal 2
      Sequel::Migrator.run(db, dir, :target=>1273253849, :migration_identities=>true)
      applied_version(db).must_equal 1
      db[:schema_migrations].select_order_map(:filename).must_equal [first_file_name.downcase]
    end

    it "verifies identities for run_single up and down" do
      dir = migration_dir
      first = write_migration(dir, first_file_name, :first_table)
      db = sqlite_database

      Sequel::TimestampMigrator.run_single(db, first, :migration_identities=>true)
      identity_rows(db).must_equal [
        {:filename=>first_file_name.downcase, :digest=>digest(first), :direction=>"up"}
      ]

      Sequel::TimestampMigrator.run_single(db, first, :direction=>:down, :migration_identities=>true)
      identity_rows(db).first[:direction].must_equal "down"
      db[:schema_migrations].count.must_equal 0
    end
  end
end
