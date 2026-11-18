# frozen_string_literal: true
require 'test/unit'
require 'pstore'
require 'tmpdir'
require 'pathname'

class PStoreJournalTest < Test::Unit::TestCase
  def setup
    @pstore_file = File.join(Dir.tmpdir, "pstore.journal.tmp.#{Process.pid}")
    @journal_file = @pstore_file + ".journal"
    @pstore = PStore.new(@pstore_file, recoverable: true)
  end

  def teardown
    Dir.glob("#{@pstore_file}*").each { |path| File.unlink(path) rescue nil }
  end

  def test_recoverable_option
    assert_equal true, PStore.new(@pstore_file, recoverable: true).recoverable?
    assert_equal false, PStore.new(@pstore_file).recoverable?
    assert_equal false, PStore.new(@pstore_file, recoverable: false).recoverable?
    assert_equal @journal_file, @pstore.journal_path
  end

  def test_successful_transaction_removes_journal
    @pstore.transaction do
      @pstore[:foo] = "bar"
      @pstore[:nested] = {a: [1, 2, 3]}
    end

    refute File.exist?(@journal_file)
    @pstore.transaction(true) do
      assert_equal "bar", @pstore[:foo]
      assert_equal({a: [1, 2, 3]}, @pstore[:nested])
    end
  end

  def test_noop_transaction_creates_no_journal
    @pstore.transaction { @pstore[:foo] = "bar" }
    @pstore.transaction { @pstore[:foo] = "bar" }
    refute File.exist?(@journal_file)
  end

  def test_aborted_transaction_creates_no_journal
    @pstore.transaction do
      @pstore[:foo] = "bar"
      @pstore.abort
    end
    refute File.exist?(@journal_file)
    @pstore.transaction(true) { assert_nil @pstore[:foo] }
  end

  def test_committed_journal_is_applied_on_reopen
    @pstore.transaction { @pstore[:v] = "old" }
    PStore::Journal.new(@pstore_file).write!(Marshal.dump({v: "new"}))

    # Read-only open must not silently apply or remove the pending journal.
    assert_raise(PStore::PendingRecoveryError) do
      @pstore.transaction(true) { }
    end
    assert File.exist?(@journal_file)

    @pstore.transaction { assert_equal "new", @pstore[:v] }
    refute File.exist?(@journal_file)
    @pstore.transaction(true) { assert_equal "new", @pstore[:v] }
  end

  def test_recovery_is_idempotent
    @pstore.transaction { @pstore[:v] = "old" }
    PStore::Journal.new(@pstore_file).write!(Marshal.dump({v: "new"}))
    @pstore.transaction { assert_equal "new", @pstore[:v] }

    # Recreate a committed journal whose image already matches the store,
    # as if cleanup was interrupted after a successful in-place write.
    image = File.binread(@pstore_file)
    PStore::Journal.new(@pstore_file).write!(image)

    writes = 0
    original = IO.instance_method(:write)
    EnvUtil.suppress_warning do
      IO.define_method(:write) do |*args|
        writes += 1 if respond_to?(:path) && path == @pstore_file
        original.bind_call(self, *args)
      end
    end
    begin
      @pstore.transaction { assert_equal "new", @pstore[:v] }
    ensure
      EnvUtil.suppress_warning { IO.define_method(:write, original) }
    end

    refute File.exist?(@journal_file)
    assert_equal 0, writes, "matching image must not be rewritten"
    assert_equal image, File.binread(@pstore_file)
  end

  def test_uncommitted_tail_is_never_read_as_a_table
    @pstore.transaction { @pstore[:v] = "old" }
    File.binwrite(@journal_file, "PStJrnl" + [1].pack("C") + [99].pack("Q>") + "partial-data")

    # Read-only open ignores the tail and reads the real store.
    @pstore.transaction(true) { assert_equal "old", @pstore[:v] }
    assert File.exist?(@journal_file), "read-only open must not remove the tail"

    # Read-write open discards the tail.
    @pstore.transaction { assert_equal "old", @pstore[:v] }
    refute File.exist?(@journal_file)
  end

  def test_empty_journal_is_an_uncommitted_tail
    @pstore.transaction { @pstore[:v] = "old" }
    File.binwrite(@journal_file, "")
    @pstore.transaction { assert_equal "old", @pstore[:v] }
    refute File.exist?(@journal_file)
  end

  def test_truncated_commit_marker_is_an_uncommitted_tail
    @pstore.transaction { @pstore[:v] = "old" }
    PStore::Journal.new(@pstore_file).write!(Marshal.dump({v: "new"}))
    full = File.binread(@journal_file)
    File.binwrite(@journal_file, full.byteslice(0, full.bytesize - 1))

    @pstore.transaction { assert_equal "old", @pstore[:v] }
    refute File.exist?(@journal_file)
  end

  def test_corrupt_committed_journal_is_terminal_and_evidence_is_preserved
    @pstore.transaction { @pstore[:v] = "old" }
    PStore::Journal.new(@pstore_file).write!(Marshal.dump({v: "new"}))
    raw = File.binread(@journal_file)
    raw.setbyte(20, raw.getbyte(20) ^ 0xff)
    File.binwrite(@journal_file, raw)

    # Read-only open: terminal error, journal left in place.
    error = assert_raise(PStore::CorruptJournalError) do
      @pstore.transaction(true) { }
    end
    assert_nil error.evidence_path
    assert File.exist?(@journal_file)

    # Read-write open: journal quarantined, store never modified.
    error = assert_raise(PStore::CorruptJournalError) do
      @pstore.transaction { }
    end
    refute File.exist?(@journal_file)
    assert File.exist?(error.evidence_path)
    @pstore.transaction(true) { assert_equal "old", @pstore[:v] }
  end

  def test_journal_write_disk_error_fails_transaction_and_keeps_store
    @pstore.transaction { @pstore[:v] = "old" }
    journal = @journal_file
    with_redefined(File, :open) do |original|
      File.define_singleton_method(:open) do |path, **options, &block|
        raise Errno::EIO if path == journal
        original.call(path, **options, &block)
      end
      assert_raise(Errno::EIO) do
        @pstore.transaction { @pstore[:v] = "new" }
      end
    end

    @pstore.transaction(true) { assert_equal "old", @pstore[:v] }
  end

  def test_in_place_write_disk_error_is_recovered_on_reopen
    @pstore.transaction { @pstore[:v] = "old" }
    store = @pstore_file
    original = IO.instance_method(:write)
    EnvUtil.suppress_warning do
      IO.define_method(:write) do |*args|
        if respond_to?(:path) && path == store && args.first.bytesize > 50
          raise Errno::ENOSPC
        end
        original.bind_call(self, *args)
      end
    end
    begin
      assert_raise(Errno::ENOSPC) do
        @pstore.transaction { @pstore[:v] = "x" * 200 }
      end
    ensure
      EnvUtil.suppress_warning { IO.define_method(:write, original) }
    end

    assert File.exist?(@journal_file), "committed journal must survive the crash"
    @pstore.transaction { assert_equal "x" * 200, @pstore[:v] }
    refute File.exist?(@journal_file)
  end

  def test_journal_cleanup_failure_is_terminal_and_retries
    @pstore.transaction { @pstore[:v] = "old" }
    journal = @journal_file
    with_redefined(File, :unlink) do |original|
      File.define_singleton_method(:unlink) do |*paths|
        raise Errno::EACCES if paths.first == journal
        original.call(*paths)
      end
      error = assert_raise(PStore::JournalCleanupError) do
        @pstore.transaction { @pstore[:v] = "new" }
      end
      assert_equal journal, error.journal_path
    end

    assert File.exist?(@journal_file), "journal stays as evidence"
    @pstore.transaction { assert_equal "new", @pstore[:v] }
    refute File.exist?(@journal_file)
  end

  def test_quarantine_failure_is_terminal_and_keeps_evidence
    @pstore.transaction { @pstore[:v] = "old" }
    PStore::Journal.new(@pstore_file).write!(Marshal.dump({v: "new"}))
    raw = File.binread(@journal_file)
    raw.setbyte(20, raw.getbyte(20) ^ 0xff)
    File.binwrite(@journal_file, raw)

    with_redefined(File, :rename) do |original|
      File.define_singleton_method(:rename) { |*_args| raise Errno::EACCES }
      assert_raise(PStore::JournalCleanupError) do
        @pstore.transaction { }
      end
    end

    assert File.exist?(@journal_file), "original corrupt journal must remain untouched"
    # Read-only open also refuses the corrupt journal but leaves it in place.
    assert_raise(PStore::CorruptJournalError) do
      @pstore.transaction(true) { }
    end
    assert File.exist?(@journal_file)
  end

  def test_journal_is_per_store
    other = File.join(Dir.tmpdir, "pstore.journal.other.#{Process.pid}")
    begin
      PStore.new(@pstore_file, recoverable: true).transaction { |s| s[:a] = 1 }
      PStore.new(other, recoverable: true).transaction { |s| s[:b] = 2 }

      assert_equal @pstore_file + ".journal", PStore::Journal.path_for(@pstore_file)
      assert_equal other + ".journal", PStore::Journal.path_for(other)
      assert_not_equal PStore::Journal.path_for(@pstore_file),
                       PStore::Journal.path_for(other)

      # A journal belonging to another store never affects this one.
      PStore::Journal.new(other).write!(Marshal.dump({b: 99}))
      @pstore.transaction(true) { assert_equal 1, @pstore[:a] }
    ensure
      Dir.glob("#{other}*").each { |path| File.unlink(path) rescue nil }
    end
  end

  def test_non_recoverable_store_ignores_journal_files
    @pstore.transaction { @pstore[:v] = "old" }
    PStore::Journal.new(@pstore_file).write!(Marshal.dump({v: "new"}))

    plain = PStore.new(@pstore_file)
    assert_equal false, plain.recoverable?
    plain.transaction(true) { assert_equal "old", plain[:v] }
    assert File.exist?(@journal_file), "plain store must not touch the journal"
  end

  def test_binary_and_deeply_nested_data_round_trips_through_recovery
    payload = (0..255).map(&:chr).join
    @pstore.transaction { @pstore[:v] = "seed" }
    image = Marshal.dump({bin: payload, nested: [[{k: (1..50).to_a}]], del: nil})
    PStore::Journal.new(@pstore_file).write!(image)

    @pstore.transaction { }
    @pstore.transaction(true) do
      assert_equal payload, @pstore[:bin]
      assert_equal [[{k: (1..50).to_a}]], @pstore[:nested]
    end
  end

  def test_recovery_on_store_that_was_never_written
    PStore::Journal.new(@pstore_file).write!(Marshal.dump({first: true}))
    refute File.exist?(@pstore_file)

    @pstore.transaction { assert_equal true, @pstore[:first] }
    refute File.exist?(@journal_file)
  end

  def test_checksum_valid_journal_with_unparseable_payload_is_terminal
    @pstore.transaction { @pstore[:v] = "old" }
    PStore::Journal.new(@pstore_file).write!("not-marshal-data")

    # Read-only open only reports that recovery is pending; it never parses
    # the payload and never removes evidence.
    assert_raise(PStore::PendingRecoveryError) do
      @pstore.transaction(true) { }
    end
    assert File.exist?(@journal_file), "read-only open must leave evidence in place"

    # Read-write open validates the payload, quarantines the journal and
    # preserves the store.
    error = assert_raise(PStore::CorruptJournalError) do
      @pstore.transaction { }
    end
    assert File.exist?(error.evidence_path), "bad journal must be preserved"
    refute File.exist?(@journal_file)
    @pstore.transaction(true) { assert_equal "old", @pstore[:v] }
  end

  def test_checksum_valid_journal_with_non_hash_payload_is_terminal
    @pstore.transaction { @pstore[:v] = "old" }
    PStore::Journal.new(@pstore_file).write!(Marshal.dump([1, 2, 3]))

    assert_raise(PStore::CorruptJournalError) do
      @pstore.transaction { }
    end
    assert_equal 1, Dir.glob("#{@journal_file}.corrupt.*").size
    @pstore.transaction(true) { assert_equal "old", @pstore[:v] }
  end

  private

  def with_redefined(mod, method_name)
    original = mod.method(method_name)
    EnvUtil.suppress_warning { yield original }
  ensure
    EnvUtil.suppress_warning do
      mod.define_singleton_method(method_name, original) if original
    end
  end
end

