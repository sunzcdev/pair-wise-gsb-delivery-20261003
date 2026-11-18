# frozen_string_literal: true
require 'test/unit'
require 'pstore'
require 'tmpdir'
require 'pathname'

class PStoreTest < Test::Unit::TestCase
  def setup
    @pstore_file = File.join(Dir.tmpdir, "pstore.tmp.#{Process.pid}")
    @pstore = PStore.new(@pstore_file)
  end

  def teardown
    File.unlink(@pstore_file) rescue nil
  end

  def test_opening_new_file_in_readonly_mode_should_result_in_empty_values
    @pstore.transaction(true) do
      assert_nil @pstore[:foo]
      assert_nil @pstore[:bar]
    end
  end

  def test_opening_new_file_in_readwrite_mode_should_result_in_empty_values
    @pstore.transaction do
      assert_nil @pstore[:foo]
      assert_nil @pstore[:bar]
    end
  end

  def test_data_should_be_loaded_correctly_when_in_readonly_mode
    @pstore.transaction do
      @pstore[:foo] = "bar"
    end
    @pstore.transaction(true) do
      assert_equal "bar", @pstore[:foo]
    end
  end

  def test_data_should_be_loaded_correctly_when_in_readwrite_mode
    @pstore.transaction do
      @pstore[:foo] = "bar"
    end
    @pstore.transaction do
      assert_equal "bar", @pstore[:foo]
    end
  end

  def test_changes_after_commit_are_discarded
    @pstore.transaction do
      @pstore[:foo] = "bar"
      @pstore.commit
      @pstore[:foo] = "baz"
    end
    @pstore.transaction(true) do
      assert_equal "bar", @pstore[:foo]
    end
  end

  def test_changes_are_not_written_on_abort
    @pstore.transaction do
      @pstore[:foo] = "bar"
      @pstore.abort
    end
    @pstore.transaction(true) do
      assert_nil @pstore[:foo]
    end
  end

  def test_data_should_be_stored_correctly_when_in_ultra_safe_mode
    @pstore.ultra_safe = true
    @pstore.transaction do
      @pstore[:foo] = "bar"
    end
    @pstore.transaction(true) do
      assert_equal "bar", @pstore[:foo]
    end
  end

  def test_ultra_safe_mode_preserves_file_mode
    omit("ultra_safe mode is not supported on Windows") if Gem.win_platform?

    @pstore.transaction { |store| store[:foo] = "bar" }
    File.chmod(0o600, @pstore_file)
    @pstore.ultra_safe = true

    original_umask = File.umask(0o000)
    @pstore.transaction { |store| store[:foo] = "baz" }

    assert_equal 0o600, File.stat(@pstore_file).mode & 0o7777
  ensure
    File.umask(original_umask) if original_umask
  end

  def test_writing_inside_readonly_transaction_raises_error
    assert_raise(PStore::Error) do
      @pstore.transaction(true) do
        @pstore[:foo] = "bar"
      end
    end
  end

  def test_thread_safe
    q1 = Thread::Queue.new
    assert_raise(PStore::Error) do
      th = Thread.new do
        @pstore.transaction do
          @pstore[:foo] = "bar"
          q1.push true
          sleep
        end
      end
      begin
        q1.pop
        @pstore.transaction {}
      ensure
        th.kill
        th.join
      end
    end
    q2 = Thread::Queue.new
    begin
      pstore = PStore.new(second_file, true)
      cur = Thread.current
      th = Thread.new do
        pstore.transaction do
          pstore[:foo] = "bar"
          q1.push true
          q2.pop
          # wait for cur to enter a transaction
          sleep 0.1 until cur.stop?
        end
      end
      begin
        q1.pop
        q2.push true
        assert_equal("bar", pstore.transaction { pstore[:foo] })
      ensure
        th.join
      end
    end
  ensure
    File.unlink(second_file) rescue nil
  end

  def test_thread_safe_argument
    assert_equal false, PStore.new(@pstore_file).instance_variable_get(:@thread_safe)
    assert_equal true, PStore.new(@pstore_file, true).instance_variable_get(:@thread_safe)
    assert_equal true, PStore.new(@pstore_file, thread_safe: true).instance_variable_get(:@thread_safe)
    assert_equal false, PStore.new(@pstore_file, true, thread_safe: false).instance_variable_get(:@thread_safe)
  end

  def test_ultra_safe_argument
    assert_equal false, PStore.new(@pstore_file).ultra_safe
    assert_equal true, PStore.new(@pstore_file, ultra_safe: true).ultra_safe
    assert_equal false, PStore.new(@pstore_file, ultra_safe: false).ultra_safe
  end

  def test_follows_symlink_by_default
    Dir.mktmpdir do |dir|
      target = File.join(dir, "target")
      link = File.join(dir, "link")
      PStore.new(target).transaction { |store| store[:key] = "value" }
      File.symlink(target, link)

      assert_equal "value", PStore.new(link).transaction(true) { |store| store[:key] }
    end
  end

  def test_does_not_follow_symlink_when_disabled
    omit("O_NOFOLLOW is not supported") unless nofollow_supported?

    Dir.mktmpdir do |dir|
      target = File.join(dir, "target")
      link = File.join(dir, "link")
      File.write(target, "")
      File.symlink(target, link)

      assert_raise(Errno::ELOOP) do
        PStore.new(link, follow_symlink: false).transaction {}
      end
      assert_empty File.read(target)
    end
  end

  def test_nested_transaction_raises_error
    assert_raise(PStore::Error) do
      @pstore.transaction { @pstore.transaction { } }
    end
    pstore = PStore.new(second_file, true)
    assert_raise(PStore::Error) do
      pstore.transaction { pstore.transaction { } }
    end
  ensure
    File.unlink(second_file) rescue nil
  end

  # Test that PStore's file operations do not blow up when default encodings are set
  def test_pstore_files_are_accessed_as_binary_files
    bug5311 = '[ruby-core:39503]'
    n = 128
    top_dir = File.expand_path('../lib', __dir__)
    assert_in_out_err(["-Eutf-8:utf-8", "-I#{top_dir}", "-rpstore", "-", @pstore_file], <<-SRC, [bug5311], [], bug5311, timeout: 30)
      @pstore = PStore.new(ARGV[0])
      (1..#{n}).each do |i|
        @pstore.transaction {@pstore["Key\#{i}"] = "value \#{i}"}
      end
      @pstore.transaction {@pstore["Bug5311"] = '#{bug5311}'}
      puts @pstore.transaction {@pstore["Bug5311"]}
    SRC
    assert_equal(bug5311, @pstore.transaction {@pstore["Bug5311"]}, bug5311)
  end

  def test_key_p
    [:key?, :root?].each do |method|
      clear_store
      @pstore.transaction do
        @pstore[:foo] = 0
        assert_equal(true, @pstore.send(method, :foo))
        assert_equal(false, @pstore.send(method, :bar))
      end
    end
  end

  def test_keys
    [:keys, :roots].each do |method|
      clear_store
      @pstore.transaction do
        assert_equal([], @pstore.send(method))
      end
      @pstore.transaction do
        @pstore[:foo] = 0
        assert_equal([:foo], @pstore.send(method))
      end
    end
  end

  def clear_store
    @pstore.transaction do
      @pstore.keys.each do |key|
        @pstore.delete(key)
      end
    end
  end

  def nofollow_supported?
    return false unless IO.const_defined?(:NOFOLLOW)

    Dir.mktmpdir do |dir|
      target = File.join(dir, "target")
      link = File.join(dir, "link")
      File.write(target, "")
      File.symlink(target, link)
      File.open(link, IO::RDONLY|IO::NOFOLLOW).close
    end
    false
  rescue Errno::ELOOP
    true
  rescue NotImplementedError, SystemCallError
    false
  end

  def second_file
    File.join(Dir.tmpdir, "pstore.tmp2.#{Process.pid}")
  end

  def test_truncated_read_only_store_is_not_empty
    @pstore.transaction { @pstore[:foo] = "bar" }
    data = File.binread(@pstore_file)
    File.binwrite(@pstore_file, data.byteslice(0, data.bytesize - 1))

    assert_raise(EOFError) { @pstore.transaction(true) { @pstore[:foo] } }
  end

  def test_store_operations_require_transaction_owner
    ready = Thread::Queue.new
    release = Thread::Queue.new
    owner = Thread.new do
      @pstore.transaction do
        @pstore[:foo] = "bar"
        ready << true
        release.pop
      end
    end

    ready.pop
    assert_raise(PStore::Error) { @pstore[:foo] }
    assert_raise(PStore::Error) { @pstore[:foo] = "other" }
  ensure
    release << true if release
    owner.join if owner
  end

  def test_commit_targets_the_owning_store
    inner = PStore.new(second_file)
    @pstore.transaction do
      @pstore[:outer] = true
      inner.transaction do
        inner[:inner] = true
        @pstore.commit
        flunk("outer commit should exit its own transaction")
      end
      flunk("outer commit should exit its own transaction")
    end

    assert_equal(true, @pstore.transaction(true) { @pstore[:outer] })
    assert_nil(inner.transaction(true) { inner[:inner] })
  ensure
    File.unlink(second_file) rescue nil
  end

  def test_fetch_compares_missing_default_by_identity
    default = Object.new
    def default.==(_other)
      true
    end

    assert_same(default, @pstore.transaction(true) { @pstore.fetch(:missing, default) })
  end

  def test_lock_retries_when_atomic_save_replaces_file
    fake_file = Struct.new(:closed) do
      def flock(_mode)
        true
      end

      def close
        self.closed = true
      end
    end
    first = fake_file.new(false)
    second = fake_file.new(false)
    files = [first, second]
    original_new = File.method(:new)
    original_identical = File.method(:identical?)
    EnvUtil.suppress_warning do
      File.define_singleton_method(:new) { |*_args, **_kwargs| files.shift }
      File.define_singleton_method(:identical?) { |file, _path| file.equal?(second) }
    end

    result = @pstore.send(:open_and_lock_file, @pstore_file, false)

    assert_same(second, result)
    assert_equal(true, first.closed)
    assert_equal(false, second.closed)
  ensure
    result.close if result && !result.closed
    EnvUtil.suppress_warning do
      File.define_singleton_method(:new, original_new) if original_new
      File.define_singleton_method(:identical?, original_identical) if original_identical
    end
  end

  def test_interrupted_atomic_save_removes_temporary_file
    return if /mswin|mingw|bccwin|wince/ =~ RUBY_PLATFORM

    @pstore.transaction { @pstore[:foo] = "old" }
    @pstore.ultra_safe = true
    original_new = File.method(:new)
    store_path = @pstore_file
    EnvUtil.suppress_warning do
      File.define_singleton_method(:new) do |path, **options|
        file = original_new.call(path, **options)
        if path.start_with?("#{store_path}.tmp.")
          file.define_singleton_method(:write) { |_data| raise Interrupt }
        end
        file
      end
    end

    assert_raise(Interrupt) { @pstore.transaction { @pstore[:foo] = "new" } }
    assert_equal([], Dir.glob("#{@pstore_file}.tmp.*"))
    assert_equal("old", @pstore.transaction(true) { @pstore[:foo] })
  ensure
    EnvUtil.suppress_warning do
      File.define_singleton_method(:new, original_new) if original_new
    end
    Dir.glob("#{@pstore_file}.tmp.*").each { |path| File.unlink(path) rescue nil }
  end

  def test_path_like_filename_is_normalized
    store = PStore.new(Pathname(@pstore_file))
    store.ultra_safe = true
    store.transaction { store[:foo] = "bar" }

    assert_instance_of(String, store.path)
    assert_equal(@pstore_file, store.path)
    assert_equal("bar", store.transaction(true) { store[:foo] })
  end

  def test_snapshot_returns_a_frozen_read_only_view_of_the_contents
    @pstore.transaction do
      @pstore[:foo] = "bar"
      @pstore[:nested] = {a: [1, 2, 3]}
    end

    snapshot = @pstore.snapshot

    assert_instance_of(PStore::Snapshot, snapshot)
    assert_equal(true, snapshot.frozen?)
    assert_equal(@pstore_file, snapshot.path)
    assert_equal("bar", snapshot[:foo])
    assert_nil(snapshot[:missing])
    assert_equal({a: [1, 2, 3]}, snapshot[:nested])
    assert_equal([:foo, :nested], snapshot.keys)
    assert_equal([:foo, :nested], snapshot.roots)
    assert_equal(true, snapshot.key?(:foo))
    assert_equal(true, snapshot.root?(:foo))
    assert_equal(false, snapshot.key?(:missing))
    assert_equal(2, snapshot.size)
    assert_equal(2, snapshot.length)
    assert_equal([1, 2, 3], snapshot.dig(:nested, :a))
    assert_equal("default", snapshot.fetch(:missing, "default"))
    assert_raise(PStore::Error) { snapshot.fetch(:missing) }
    assert_equal({foo: "bar", nested: {a: [1, 2, 3]}}, snapshot.to_h)
    assert_equal([[:foo, "bar"], [:nested, {a: [1, 2, 3]}]],
                 snapshot.map { |key, value| [key, value] })
    assert_equal([:foo, :nested], snapshot.each.map { |key, _value| key })
    assert_not_respond_to(snapshot, :[]=)
    assert_not_respond_to(snapshot, :delete)
    assert_raise(FrozenError) { snapshot.instance_variable_set(:@table, {}) }
  end

  def test_snapshot_of_a_missing_file_is_empty
    missing = File.join(Dir.tmpdir, "pstore.missing.#{Process.pid}.#{object_id}")
    snapshot = PStore.new(missing).snapshot

    assert_instance_of(PStore::Snapshot, snapshot)
    assert_equal([], snapshot.keys)
    assert_equal(0, snapshot.size)
    assert_nil(snapshot[:anything])
  ensure
    File.unlink(missing) rescue nil
  end

  def test_snapshot_does_not_share_mutable_objects_with_transactions
    @pstore.transaction do
      @pstore[:list] = [1, 2]
      @pstore[:text] = "value"
    end

    snapshot = @pstore.snapshot
    snapshot[:list] << 3
    snapshot[:text].upcase!

    @pstore.transaction(true) do
      assert_equal([1, 2], @pstore[:list])
      assert_equal("value", @pstore[:text])
    end

    @pstore.transaction do
      @pstore[:list] << 99
      @pstore[:text] << "!"
    end

    assert_equal([1, 2, 3], snapshot[:list])
    assert_equal("VALUE", snapshot[:text])
  end

  def test_snapshot_preserves_aliasing_between_entries
    shared = ["shared"]
    @pstore.transaction do
      @pstore[:one] = shared
      @pstore[:two] = {inner: shared}
    end

    snapshot = @pstore.snapshot

    assert_same(snapshot[:one], snapshot[:two][:inner])
    snapshot[:one] << "changed"
    assert_equal(["shared", "changed"], snapshot[:two][:inner])
    @pstore.transaction(true) do
      assert_equal(["shared"], @pstore[:one])
    end
  end

  def test_snapshot_preserves_cyclic_structures
    cyclic = {name: "root"}
    cyclic[:self] = cyclic
    nested = [cyclic]
    cyclic[:list] = nested
    @pstore.transaction { @pstore[:cycle] = cyclic }

    snapshot = @pstore.snapshot
    snap_cycle = snapshot[:cycle]

    assert_same(snap_cycle, snap_cycle[:self])
    assert_same(snap_cycle, snap_cycle[:list][0])
    assert_equal("root", snap_cycle[:name])
    @pstore.transaction(true) do
      assert_not_same(snap_cycle, @pstore[:cycle])
      assert_same(@pstore[:cycle], @pstore[:cycle][:self])
    end
  end

  def test_snapshot_is_fixed_and_does_not_reflect_later_updates
    @pstore.transaction { @pstore[:foo] = "old" }
    snapshot = @pstore.snapshot

    @pstore.transaction do
      @pstore[:foo] = "new"
      @pstore[:bar] = "added"
    end

    assert_equal("old", snapshot[:foo])
    assert_nil(snapshot[:bar])
    assert_equal([:foo], snapshot.keys)

    later = @pstore.snapshot
    assert_equal("new", later[:foo])
    assert_equal("added", later[:bar])
    assert_equal([:foo, :bar], later.keys)
  end

  def test_snapshot_releases_the_store_lock_before_returning
    @pstore.transaction { @pstore[:foo] = "bar" }

    @pstore.snapshot

    # A following write transaction must be able to acquire every lock.
    @pstore.transaction do
      @pstore[:foo] = "baz"
    end
    assert_equal("baz", @pstore.transaction(true) { @pstore[:foo] })
  end

  def test_snapshot_does_not_block_write_transactions_on_another_instance
    @pstore.transaction { @pstore[:foo] = "bar" }
    snapshot = @pstore.snapshot
    other = PStore.new(@pstore_file, true)
    writer_done = Thread::Queue.new

    writer = Thread.new do
      other.transaction do
        other[:foo] = "written while snapshot is held"
        writer_done << true
      end
    end

    assert(writer.join(10), "write transaction blocked while snapshot was held")
    assert_equal(true, writer_done.pop)
    assert_equal("written while snapshot is held",
                 @pstore.transaction(true) { @pstore[:foo] })
    assert_equal("bar", snapshot[:foo])
  ensure
    writer&.join
  end

  def test_snapshot_traversal_can_run_while_a_write_transaction_holds_the_lock
    @pstore.transaction do
      @pstore[:items] = (1..100).to_a
      @pstore[:label] = "before"
    end
    other = PStore.new(@pstore_file, true)
    entered = Thread::Queue.new
    release = Thread::Queue.new
    # The snapshot is fully built and detached before any lock is held
    # by the writer below.
    snapshot = @pstore.snapshot

    holder = Thread.new do
      other.transaction do
        other[:label] = "after"
        entered << true
        release.pop
      end
    end

    entered.pop
    assert_equal(true, holder.alive?)
    # Neither the store mutex nor the file lock may be needed here.
    traversed = []
    snapshot.each { |key, value| traversed << [key, value] }
    assert_equal([[:items, (1..100).to_a], [:label, "before"]].sort, traversed.sort)
    assert_equal(true, holder.alive?)
    assert_equal("before", snapshot[:label])
  ensure
    release << true if release
    holder&.join
  end

  def test_two_instances_interleave_snapshot_reads_and_writes
    writer = PStore.new(@pstore_file, true)
    reader = PStore.new(@pstore_file, true)

    writer.transaction { writer[:counter] = 0 }
    snapshots = []

    5.times do |i|
      writer.transaction do
        writer[:counter] = i + 1
        writer[:history] ||= []
        writer[:history] << i
      end
      snapshots << reader.snapshot
    end

    snapshots.each_with_index do |snapshot, i|
      assert_equal(i + 1, snapshot[:counter])
      assert_equal((0..i).to_a, snapshot[:history])
    end
    assert_equal(5, writer.transaction(true) { writer[:counter] })
    # Each snapshot is still its own fixed copy.
    assert_equal(1, snapshots.first[:counter])
    assert_not_same(snapshots.first, snapshots.last)
  end

  def test_read_only_transaction_keeps_holding_the_lock_for_its_block
    # The file must exist so that the read-only transaction actually
    # acquires a shared file lock.
    @pstore.transaction { @pstore[:seed] = true }
    pstore = PStore.new(@pstore_file, true)
    other = PStore.new(@pstore_file, true)
    entered = Thread::Queue.new
    release = Thread::Queue.new

    reader = Thread.new do
      pstore.transaction(true) do
        entered << true
        release.pop
      end
    end

    entered.pop
    writer = Thread.new do
      other.transaction { other[:foo] = "bar" }
    end

    assert_nil(writer.join(0.5))
  ensure
    release << true if release
    reader&.join
    assert(writer&.join(10))
  end

  def test_snapshot_raises_inside_a_transaction
    @pstore.transaction do
      assert_raise(PStore::Error) { @pstore.snapshot }
    end

    thread_safe = PStore.new(second_file, true)
    begin
      thread_safe.transaction do
        assert_raise(PStore::Error) { thread_safe.snapshot }
      end
    ensure
      File.unlink(second_file) rescue nil
    end
  end

  def test_failed_snapshot_does_not_return_a_partial_view_and_releases_locks
    @pstore.transaction { @pstore[:foo] = "bar" }
    data = File.binread(@pstore_file)
    File.binwrite(@pstore_file, data.byteslice(0, data.bytesize - 1))

    result = nil
    assert_raise(EOFError) { result = @pstore.snapshot }
    assert_nil(result)

    # Restore valid data.  If the store mutex were still held, this
    # transaction would raise PStore::Error ("nested transaction").
    File.binwrite(@pstore_file, data)
    @pstore.transaction do
      @pstore[:foo] = "rewritten"
    end
    assert_equal("rewritten", @pstore.snapshot[:foo])
  ensure
    File.binwrite(@pstore_file, data) if defined?(data) && data
  end

  def test_snapshot_to_h_is_a_disconnected_shallow_copy
    @pstore.transaction { @pstore[:foo] = [1] }
    snapshot = @pstore.snapshot

    copy = snapshot.to_h
    copy[:extra] = true

    assert_not_same(copy, snapshot.to_h)
    assert_nil(snapshot[:extra])
    assert_equal([:foo], snapshot.keys)
  end
end
