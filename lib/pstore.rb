# frozen_string_literal: true
# = PStore -- Transactional File Storage for Ruby Objects
#
# pstore.rb -
#   originally by matz
#   documentation by Kev Jackson and James Edward Gray II
#   improved by Hongli Lai
#
# See PStore for documentation.

require "digest"

# \PStore implements a file based persistence mechanism based on a Hash.
# User code can store hierarchies of Ruby objects (values)
# into the data store by name (keys).
# An object hierarchy may be just a single object.
# User code may later read values back from the data store
# or even update data, as needed.
#
# The transactional behavior ensures that any changes succeed or fail together.
# This can be used to ensure that the data store is not left in a transitory state,
# where some values were updated but others were not.
#
# Behind the scenes, Ruby objects are stored to the data store file with Marshal.
# That carries the usual limitations. Proc objects cannot be marshalled,
# for example.
#
# There are three important concepts here (details at the links):
#
# - {Store}[rdoc-ref:PStore@The+Store]: a store is an instance of \PStore.
# - {Entries}[rdoc-ref:PStore@Entries]: the store is hash-like;
#   each entry is the key for a stored object.
# - {Transactions}[rdoc-ref:PStore@Transactions]: each transaction is a collection
#   of prospective changes to the store;
#   a transaction is defined in the block given with a call
#   to PStore#transaction.
#
# == About the Examples
#
# Examples on this page need a store that has known properties.
# They can get a new (and populated) store by calling thus:
#
#   example_store do |store|
#     # Example code using store goes here.
#   end
#
# All we really need to know about +example_store+
# is that it yields a fresh store with a known population of entries;
# its implementation:
#
#   require 'pstore'
#   require 'tempfile'
#   # Yield a pristine store for use in examples.
#   def example_store
#     # Create the store in a temporary file.
#     Tempfile.create do |file|
#       store = PStore.new(file)
#       # Populate the store.
#       store.transaction do
#         store[:foo] = 0
#         store[:bar] = 1
#         store[:baz] = 2
#       end
#       yield store
#     end
#   end
#
# == The Store
#
# The contents of the store are maintained in a file whose path is specified
# when the store is created (see PStore.new).
# The objects are stored and retrieved using
# module Marshal, which means that certain objects cannot be added to the store;
# see {Marshal::dump}[https://docs.ruby-lang.org/en/master/Marshal.html#method-c-dump].
#
# == Entries
#
# A store may have any number of entries.
# Each entry has a key and a value, just as in a hash:
#
# - Key: as in a hash, the key can be (almost) any object;
#   see {Hash Keys}[https://docs.ruby-lang.org/en/master/Hash.html#class-Hash-label-Hash+Keys].
#   You may find it convenient to keep it simple by using only
#   symbols or strings as keys.
# - Value: the value may be any object that can be marshalled by \Marshal
#   (see {Marshal::dump}[https://docs.ruby-lang.org/en/master/Marshal.html#method-c-dump])
#   and in fact may be a collection
#   (e.g., an array, a hash, a set, a range, etc).
#   That collection may in turn contain nested objects,
#   including collections, to any depth;
#   those objects must also be \Marshal-able.
#   See {Hierarchical Values}[rdoc-ref:PStore@Hierarchical+Values].
#
# == Transactions
#
# === The Transaction Block
#
# The block given with a call to method #transaction#
# contains a _transaction_,
# which consists of calls to \PStore methods that
# read from or write to the store
# (that is, all \PStore methods except #transaction itself,
# #path, and Pstore.new):
#
#   example_store do |store|
#     store.transaction do
#       store.keys # => [:foo, :bar, :baz]
#       store[:bat] = 3
#       store.keys # => [:foo, :bar, :baz, :bat]
#     end
#   end
#
# Execution of the transaction is deferred until the block exits,
# and is executed _atomically_ (all-or-nothing):
# either all transaction calls are executed, or none are.
# This maintains the integrity of the store.
#
# Other code in the block (including even calls to #path and PStore.new)
# is executed immediately, not deferred.
#
# The transaction block:
#
# - May not contain a nested call to #transaction.
# - Is the only context where methods that read from or write to
#   the store are allowed.
#
# As seen above, changes in a transaction are made automatically
# when the block exits.
# The block may be exited early by calling method #commit or #abort.
#
# - Method #commit triggers the update to the store and exits the block:
#
#     example_store do |store|
#       store.transaction do
#         store.keys # => [:foo, :bar, :baz]
#         store[:bat] = 3
#         store.commit
#         fail 'Cannot get here'
#       end
#       store.transaction do
#         # Update was completed.
#         store.keys # => [:foo, :bar, :baz, :bat]
#       end
#     end
#
# - Method #abort discards the update to the store and exits the block:
#
#     example_store do |store|
#       store.transaction do
#         store.keys # => [:foo, :bar, :baz]
#         store[:bat] = 3
#         store.abort
#         fail 'Cannot get here'
#       end
#       store.transaction do
#         # Update was not completed.
#         store.keys # => [:foo, :bar, :baz]
#       end
#     end
#
# === Read-Only Transactions
#
# By default, a transaction allows both reading from and writing to
# the store:
#
#   store.transaction do
#     # Read-write transaction.
#     # Any code except a call to #transaction is allowed here.
#   end
#
# If argument +read_only+ is passed as +true+,
# only reading is allowed:
#
#   store.transaction(true) do
#     # Read-only transaction:
#     # Calls to #transaction, #[]=, and #delete are not allowed here.
#   end
#
# == Hierarchical Values
#
# The value for an entry may be a simple object (as seen above).
# It may also be a hierarchy of objects nested to any depth:
#
#   deep_store = PStore.new('deep.store')
#   deep_store.transaction do
#     array_of_hashes = [{}, {}, {}]
#     deep_store[:array_of_hashes] = array_of_hashes
#     deep_store[:array_of_hashes] # => [{}, {}, {}]
#     hash_of_arrays = {foo: [], bar: [], baz: []}
#     deep_store[:hash_of_arrays] = hash_of_arrays
#     deep_store[:hash_of_arrays]  # => {:foo=>[], :bar=>[], :baz=>[]}
#     deep_store[:hash_of_arrays][:foo].push(:bat)
#     deep_store[:hash_of_arrays]  # => {:foo=>[:bat], :bar=>[], :baz=>[]}
#   end
#
# And recall that you can use
# {dig methods}[https://docs.ruby-lang.org/en/master/dig_methods_rdoc.html]
# in a returned hierarchy of objects.
#
# == Working with the Store
#
# === Creating a Store
#
# Use method PStore.new to create a store.
# The new store creates or opens its containing file:
#
#   store = PStore.new('t.store')
#
# === Modifying the Store
#
# Use method #[]= to update or create an entry:
#
#   example_store do |store|
#     store.transaction do
#       store[:foo] = 1 # Update.
#       store[:bam] = 1 # Create.
#     end
#   end
#
# Use method #delete to remove an entry:
#
#   example_store do |store|
#     store.transaction do
#       store.delete(:foo)
#       store[:foo] # => nil
#     end
#   end
#
# === Retrieving Values
#
# Use method #fetch (allows default) or #[] (defaults to +nil+)
# to retrieve an entry:
#
#   example_store do |store|
#     store.transaction do
#       store[:foo]             # => 0
#       store[:nope]            # => nil
#       store.fetch(:baz)       # => 2
#       store.fetch(:nope, nil) # => nil
#       store.fetch(:nope)      # Raises exception.
#     end
#   end
#
# === Querying the Store
#
# Use method #key? to determine whether a given key exists:
#
#   example_store do |store|
#     store.transaction do
#       store.key?(:foo) # => true
#     end
#   end
#
# Use method #keys to retrieve keys:
#
#   example_store do |store|
#     store.transaction do
#       store.keys # => [:foo, :bar, :baz]
#     end
#   end
#
# Use method #path to retrieve the path to the store's underlying file;
# this method may be called from outside a transaction block:
#
#   store = PStore.new('t.store')
#   store.path # => "t.store"
#
# == Transaction Safety
#
# For transaction safety, see:
#
# - Optional argument +thread_safe+ at method PStore.new.
# - Attribute #ultra_safe.
#
# Needless to say, if you're storing valuable data with \PStore, then you should
# backup the \PStore file from time to time.
#
# == An Example Store
#
#  require "pstore"
#
#  # A mock wiki object.
#  class WikiPage
#
#    attr_reader :page_name
#
#    def initialize(page_name, author, contents)
#      @page_name = page_name
#      @revisions = Array.new
#      add_revision(author, contents)
#    end
#
#    def add_revision(author, contents)
#      @revisions << {created: Time.now,
#                     author: author,
#                     contents: contents}
#    end
#
#    def wiki_page_references
#      [@page_name] + @revisions.last[:contents].scan(/\b(?:[A-Z]+[a-z]+){2,}/)
#    end
#
#  end
#
#  # Create a new wiki page.
#  home_page = WikiPage.new("HomePage", "James Edward Gray II",
#                           "A page about the JoysOfDocumentation..." )
#
#  wiki = PStore.new("wiki_pages.pstore")
#  # Update page data and the index together, or not at all.
#  wiki.transaction do
#    # Store page.
#    wiki[home_page.page_name] = home_page
#    # Create page index.
#    wiki[:wiki_index] ||= Array.new
#    # Update wiki index.
#    wiki[:wiki_index].push(*home_page.wiki_page_references)
#  end
#
#  # Read wiki data, setting argument read_only to true.
#  wiki.transaction(true) do
#    wiki.keys.each do |key|
#      puts key
#      puts wiki[key]
#    end
#  end
#
class PStore
  # :stopdoc:
  VERSION = "0.3.0"

  RDWR_ACCESS = IO::RDWR | IO::CREAT | IO::BINARY
  RD_ACCESS = IO::RDONLY | IO::BINARY
  WR_ACCESS = IO::WRONLY | IO::CREAT | IO::TRUNC | IO::BINARY
  NOFOLLOW = IO.const_defined?(:NOFOLLOW) ? IO::NOFOLLOW : 0
  private_constant :RDWR_ACCESS, :RD_ACCESS, :WR_ACCESS, :NOFOLLOW
  # :startdoc:

  # The error type thrown by all PStore methods.
  class Error < StandardError
  end

  # The error type raised when the instance mutex or the store file lock
  # could not be acquired within the configured lock timeout.
  # See PStore.new and PStore#transaction.
  class LockTimeoutError < Error
  end

  # The error type raised when waiting for the instance mutex or the
  # store file lock was cancelled through the configured cancel check.
  # See PStore.new and PStore#transaction.
  class CancelledError < Error
  end

  # Whether \PStore should do its best to prevent file corruptions,
  # even when an unlikely error (such as memory-error or filesystem error) occurs:
  #
  # - +true+: changes are posted by creating a temporary file,
  #   writing the updated data to it, then renaming the file to the given #path.
  #   File integrity is maintained.
  #   Note: has effect only if the filesystem has atomic file rename
  #   (as do POSIX platforms Linux, MacOS, FreeBSD and others).
  #
  # - +false+ (the default): changes are posted by rewinding the open file
  #   and writing the updated data.
  #   File integrity is maintained if the filesystem raises
  #   no unexpected I/O error;
  #   if such an error occurs during a write to the store,
  #   the file may become corrupted.
  #
  attr_accessor :ultra_safe

  # Returns a new \PStore object.
  #
  # Argument +file+ is the path to the file in which objects are to be stored;
  # if the file exists, it should be one that was written by \PStore.
  #
  #   path = 't.store'
  #   store = PStore.new(path)
  #
  # A \PStore object is
  # {reentrant}[https://en.wikipedia.org/wiki/Reentrancy_(computing)].
  # If argument or keyword argument +thread_safe+ is given as +true+,
  # the object is also thread-safe (at the cost of a small performance penalty):
  #
  #   store = PStore.new(path, true)
  #   store = PStore.new(path, thread_safe: true)
  #
  # If keyword argument +ultra_safe+ is given as +true+, the object is
  # set to +ultra_safe+ mode.
  #
  #   store = PStore.new(path, ultra_safe: true)
  #
  # If keyword argument +follow_symlink+ is given as +false+, the store file
  # itself must not be a symbolic link.  This option has no effect on platforms
  # that do not support IO::NOFOLLOW.  Symbolic links in parent directories are
  # not affected.
  #
  #   store = PStore.new(path, follow_symlink: false)
  #
  # If keyword argument +lock_timeout+ is given as a non-negative number,
  # a transaction waits at most that many seconds (in total) to acquire both
  # the instance mutex and the store file lock, and raises
  # PStore::LockTimeoutError when the deadline passes;
  # +nil+ (the default) means waiting without a deadline:
  #
  #   store = PStore.new(path, lock_timeout: 5)
  #
  # If keyword argument +cancel_check+ is given as a callable object,
  # it is called periodically while a transaction waits for the locks;
  # when it returns a truthy value, the wait is aborted with
  # PStore::CancelledError:
  #
  #   store = PStore.new(path, cancel_check: -> { shut_down_requested? })
  #
  # Both options can be overridden per transaction; see PStore#transaction.
  # A failed wait never starts loading or modifying the store data,
  # closes the file handle opened for the attempt, and never releases
  # a lock held by another thread or process.
  #
  def initialize(file, _thread_safe = false, thread_safe: _thread_safe,
                 ultra_safe: false, follow_symlink: true,
                 lock_timeout: nil, cancel_check: nil)
    dir = File::dirname(file)
    unless File::directory? dir
      raise PStore::Error, format("directory %s does not exist", dir)
    end
    if File::exist? file and not File::readable? file
      raise PStore::Error, format("file %s not readable", file)
    end
    validate_lock_timeout(lock_timeout)
    @filename = File.path(file)
    @abort = false
    @rdonly = false
    @ultra_safe = ultra_safe
    @thread_safe = thread_safe
    @nofollow = follow_symlink ? 0 : NOFOLLOW
    @lock_timeout = lock_timeout
    @cancel_check = cancel_check
    @lock = Thread::Mutex.new
  end

  # Raises PStore::Error if the calling code is not in a PStore#transaction.
  def in_transaction
    raise PStore::Error, "not in transaction" unless @lock.owned?
  end
  #
  # Raises PStore::Error if the calling code is not in a PStore#transaction or
  # if the code is in a read-only PStore#transaction.
  #
  def in_transaction_wr
    in_transaction
    raise PStore::Error, "in read-only transaction" if @rdonly
  end
  private :in_transaction, :in_transaction_wr

  # Returns the value for the given +key+ if the key exists.
  # +nil+ otherwise;
  # if not +nil+, the returned value is an object or a hierarchy of objects:
  #
  #   example_store do |store|
  #     store.transaction do
  #       store[:foo]  # => 0
  #       store[:nope] # => nil
  #     end
  #   end
  #
  # Returns +nil+ if there is no such key.
  #
  # See also {Hierarchical Values}[rdoc-ref:PStore@Hierarchical+Values].
  #
  # Raises an exception if called outside a transaction block.
  def [](key)
    in_transaction
    @table[key]
  end

  # Like #[], except that it accepts a default value for the store.
  # If the +key+ does not exist:
  #
  # - Raises an exception if +default+ is +PStore::Error+.
  # - Returns the value of +default+ otherwise:
  #
  #     example_store do |store|
  #       store.transaction do
  #         store.fetch(:nope, nil) # => nil
  #         store.fetch(:nope)      # Raises an exception.
  #       end
  #     end
  #
  # Raises an exception if called outside a transaction block.
  def fetch(key, default=PStore::Error)
    in_transaction
    unless @table.key? key
      if PStore::Error.equal?(default)
        raise PStore::Error, format("undefined key '%s'", key)
      else
        return default
      end
    end
    @table[key]
  end

  # Creates or replaces the value for the given +key+:
  #
  #   example_store do |store|
  #     temp.transaction do
  #       temp[:bat] = 3
  #     end
  #   end
  #
  # See also {Hierarchical Values}[rdoc-ref:PStore@Hierarchical+Values].
  #
  # Raises an exception if called outside a transaction block.
  def []=(key, value)
    in_transaction_wr
    @table[key] = value
  end

  # Removes and returns the value at +key+ if it exists:
  #
  #   example_store do |store|
  #     store.transaction do
  #       store[:bat] = 3
  #       store.delete(:bat)
  #     end
  #   end
  #
  # Returns +nil+ if there is no such key.
  #
  # Raises an exception if called outside a transaction block.
  def delete(key)
    in_transaction_wr
    @table.delete key
  end

  # Returns an array of the existing keys:
  #
  #   example_store do |store|
  #     store.transaction do
  #       store.keys # => [:foo, :bar, :baz]
  #     end
  #   end
  #
  # Raises an exception if called outside a transaction block.
  def keys
    in_transaction
    @table.keys
  end
  alias roots keys

  # Returns +true+ if +key+ exists, +false+ otherwise:
  #
  #   example_store do |store|
  #     store.transaction do
  #       store.key?(:foo) # => true
  #     end
  #   end
  #
  # Raises an exception if called outside a transaction block.
  def key?(key)
    in_transaction
    @table.key? key
  end
  alias root? key?

  # Returns the string file path used to create the store:
  #
  #   store.path # => "flat.store"
  #
  def path
    @filename
  end

  # Exits the current transaction block, committing any changes
  # specified in the
  # {transaction block}[rdoc-ref:PStore@The+Transaction+Block].
  #
  # Raises an exception if called outside a transaction block.
  def commit
    in_transaction
    @abort = false
    throw self
  end

  # Exits the current transaction block, discarding any changes
  # specified in the
  # {transaction block}[rdoc-ref:PStore@The+Transaction+Block].
  #
  # Raises an exception if called outside a transaction block.
  def abort
    in_transaction
    @abort = true
    throw self
  end

  # Opens a transaction block for the store.
  # See {Transactions}[rdoc-ref:PStore@Transactions].
  #
  # With argument +read_only+ as +false+, the block may both read from
  # and write to the store.
  #
  # With argument +read_only+ as +true+, the block may not include calls
  # to #transaction, #[]=, or #delete.
  #
  # Keyword arguments +lock_timeout+ and +cancel_check+ override the
  # defaults given to PStore.new for this transaction only; they bound
  # the total time spent waiting for the instance mutex and the store
  # file lock, and allow the wait to be cancelled.  The store data is
  # not loaded or modified before both locks are held.  A wait that
  # times out raises PStore::LockTimeoutError, a cancelled wait raises
  # PStore::CancelledError, and a corrupted store file raises PStore::Error;
  # the three conditions are reported separately.
  #
  # Raises an exception if called within a transaction block.
  def transaction(read_only = false, lock_timeout: @lock_timeout,
                  cancel_check: @cancel_check)  # :yields:  pstore
    value = nil
    deadline = lock_wait_deadline(lock_timeout)
    acquire_instance_lock(deadline, cancel_check)
    begin
      file = open_and_lock_file(@filename, read_only, deadline, cancel_check)
      # Both locks are held at this point; only now may the transaction
      # flags be set and the store data be loaded or modified.
      @rdonly = read_only
      @abort = false
      if file
        begin
          @table, checksum, original_data_size = load_data(file, read_only)

          catch(self) do
            value = yield(self)
          end

          if !@abort && !read_only
            save_data(checksum, original_data_size, file)
          end
        ensure
          file.close
        end
      else
        # This can only occur if read_only == true.
        @table = {}
        catch(self) do
          value = yield(self)
        end
      end
    ensure
      @lock.unlock
    end
    value
  end

  private
  # :stopdoc:

  # Constant for relieving Ruby's garbage collector.
  CHECKSUM_ALGO = %w[SHA512 SHA384 SHA256 SHA1 RMD160 MD5].each do |algo|
    begin
      break Digest(algo)
    rescue LoadError
    end
  end
  EMPTY_STRING = ""
  EMPTY_MARSHAL_DATA = Marshal.dump({})
  EMPTY_MARSHAL_CHECKSUM = CHECKSUM_ALGO.digest(EMPTY_MARSHAL_DATA)

  EMPTY_MARSHAL_DATA.freeze
  EMPTY_MARSHAL_CHECKSUM.freeze
  private_constant :CHECKSUM_ALGO, :EMPTY_STRING, :EMPTY_MARSHAL_DATA, :EMPTY_MARSHAL_CHECKSUM

  # Interval between attempts while polling for the instance mutex or
  # the store file lock.
  LOCK_WAIT_POLL_INTERVAL = 0.05
  private_constant :LOCK_WAIT_POLL_INTERVAL

  def validate_lock_timeout(lock_timeout) # :nodoc:
    return if lock_timeout.nil?
    unless lock_timeout.is_a?(Numeric) && lock_timeout >= 0
      raise ArgumentError, "lock_timeout must be a non-negative number or nil"
    end
  end

  # Returns the monotonic-clock deadline for a lock wait, or nil when
  # the wait has no deadline.
  def lock_wait_deadline(lock_timeout)
    validate_lock_timeout(lock_timeout)
    return nil if lock_timeout.nil?
    monotonic_clock + lock_timeout
  end

  def monotonic_clock
    Process.clock_gettime(Process::CLOCK_MONOTONIC)
  end

  def check_lock_wait_cancelled(cancel_check)
    if cancel_check && cancel_check.call
      raise CancelledError, format("cancelled while waiting for the lock on %s", @filename)
    end
  end

  def check_lock_wait_timeout(deadline)
    if deadline && monotonic_clock >= deadline
      raise LockTimeoutError, format("timed out waiting for the lock on %s", @filename)
    end
  end

  def lock_wait_pause(deadline)
    if deadline
      remaining = deadline - monotonic_clock
      check_lock_wait_timeout(deadline) if remaining <= 0
      sleep(remaining < LOCK_WAIT_POLL_INTERVAL ? remaining : LOCK_WAIT_POLL_INTERVAL)
    else
      sleep(LOCK_WAIT_POLL_INTERVAL)
    end
  end

  #
  # Acquire the instance mutex, waiting at most until _deadline_
  # (a monotonic-clock value, or nil for no deadline) and honouring
  # _cancel_check_.  A nested transaction is still reported immediately.
  #
  def acquire_instance_lock(deadline, cancel_check)
    raise PStore::Error, "nested transaction" if @lock.owned?
    if deadline.nil? && cancel_check.nil?
      if @thread_safe
        @lock.lock
      else
        raise PStore::Error, "nested transaction" unless @lock.try_lock
      end
      return
    end
    loop do
      check_lock_wait_cancelled(cancel_check)
      return if @lock.try_lock
      check_lock_wait_timeout(deadline)
      lock_wait_pause(deadline)
    end
  end

  #
  # Open the specified filename (either in read-only mode or in
  # read-write mode) and lock it for reading or writing.
  #
  # The opened File object will be returned. If _read_only_ is true,
  # and the file does not exist, then nil will be returned.
  #
  # Waiting for the file lock is bounded by _deadline_ (a monotonic-clock
  # value, or nil for no deadline) and can be aborted via _cancel_check_.
  # A failed wait closes the file handle opened here; a lock held by
  # another thread or process is never unlocked.
  #
  # All other exceptions are propagated.
  #
  def open_and_lock_file(filename, read_only, deadline = nil, cancel_check = nil)
    filename = File.path(filename)
    loop do
      check_lock_wait_cancelled(cancel_check)
      check_lock_wait_timeout(deadline)
      if read_only
        begin
          file = File.new(filename, mode: RD_ACCESS | @nofollow, encoding: Encoding::ASCII_8BIT)
        rescue Errno::ENOENT
          return nil
        end
      else
        file = File.new(filename, mode: RDWR_ACCESS | @nofollow, encoding: Encoding::ASCII_8BIT)
      end
      current = false
      begin
        lock_file(file, read_only, deadline, cancel_check)
        # An atomic save may have replaced the file while this lock was pending.
        current = File.identical?(file, filename)
        return file if current
      ensure
        file.close unless current
      end
    end
  end

  #
  # Lock the opened store file, waiting at most until _deadline_ and
  # honouring _cancel_check_.  Uses non-blocking flock attempts so the
  # wait stays interruptible; the lock is never released here.
  #
  def lock_file(file, read_only, deadline, cancel_check)
    mode = read_only ? File::LOCK_SH : File::LOCK_EX
    if deadline.nil? && cancel_check.nil?
      file.flock(mode)
      return
    end
    mode |= File::LOCK_NB
    loop do
      check_lock_wait_cancelled(cancel_check)
      return if file.flock(mode)
      check_lock_wait_timeout(deadline)
      lock_wait_pause(deadline)
    end
  end

  # Load the given PStore file.
  # If +read_only+ is true, the unmarshalled Hash will be returned.
  # If +read_only+ is false, a 3-tuple will be returned: the unmarshalled
  # Hash, a checksum of the data, and the size of the data.
  def load_data(file, read_only)
    if read_only
      begin
        table = load(file)
        raise Error, "PStore file seems to be corrupted." unless table.is_a?(Hash)
      rescue EOFError
        raise unless file.size == 0
        # This seems to be a newly-created file.
        table = {}
      end
      table
    else
      data = file.read
      if data.empty?
        # This seems to be a newly-created file.
        table = {}
        checksum = empty_marshal_checksum
        size = empty_marshal_data.bytesize
      else
        table = load(data)
        checksum = CHECKSUM_ALGO.digest(data)
        size = data.bytesize
        raise Error, "PStore file seems to be corrupted." unless table.is_a?(Hash)
      end
      data.replace(EMPTY_STRING)
      [table, checksum, size]
    end
  end

  ON_WINDOWS = true & (/mswin|mingw|bccwin|wince/ =~ RUBY_PLATFORM) # :nodoc:
  private_constant :ON_WINDOWS

  def save_data(original_checksum, original_file_size, file)
    new_data = dump(@table)

    if new_data.bytesize != original_file_size || CHECKSUM_ALGO.digest(new_data) != original_checksum
      if @ultra_safe && !ON_WINDOWS
        # Once a file is locked, Windows does not guarantee that the
        # lock will be released until the file is closed.
        save_data_with_atomic_file_rename_strategy(new_data, file)
      else
        save_data_with_fast_strategy(new_data, file)
      end
    end

    new_data.replace(EMPTY_STRING)
  end

  def save_data_with_atomic_file_rename_strategy(data, file)
    temp_filename = "#{@filename}.tmp.#{Process.pid}.#{rand 1000000}"
    temp_file = File.new(temp_filename, mode: WR_ACCESS | @nofollow, encoding: Encoding::ASCII_8BIT, perm: 0o000)
    begin
      temp_file.flock(File::LOCK_EX)
      temp_file.write(data)
      temp_file.flush
      temp_file.chmod(file.stat.mode)
      File.rename(temp_filename, @filename)
    rescue Exception
      File.unlink(temp_file) rescue nil
      raise
    ensure
      temp_file.close
    end
  end

  def save_data_with_fast_strategy(data, file)
    file.rewind
    file.write(data)
    file.truncate(data.bytesize)
  end


  # This method is just a wrapped around Marshal.dump
  # to allow subclass overriding used in YAML::Store.
  def dump(table)  # :nodoc:
    Marshal::dump(table)
  end

  # This method is just a wrapped around Marshal.load.
  # to allow subclass overriding used in YAML::Store.
  def load(content)  # :nodoc:
    Marshal::load(content)
  end

  def empty_marshal_data
    EMPTY_MARSHAL_DATA
  end
  def empty_marshal_checksum
    EMPTY_MARSHAL_CHECKSUM
  end
end
