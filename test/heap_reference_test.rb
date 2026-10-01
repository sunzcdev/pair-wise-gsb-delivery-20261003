require_relative 'test_helper'
require 'rack/cache/meta_store'
require 'rack/cache/entity_store'

# Proofs for body (entity) reclamation driven by reference management
# between the in-memory Heap MetaStore and EntityStore. Two cache entries
# that serialize to the same body share one entity; the entity must be
# reclaimed at exactly the moment its last referencing entry disappears -
# never on invalidation, never while a reader is iterating it, and never
# leaving dangling references when a metadata write fails.
describe 'Heap store reference management' do
  before do
    @meta = Rack::Cache::MetaStore::Heap.new
    @entities = Rack::Cache::EntityStore::Heap.new
  end

  def mock_request(uri = 'http://example.com/test', opts = {})
    env = Rack::MockRequest.env_for(uri, opts)
    Rack::Cache::Request.new(env)
  end

  def mock_response(body = ['test'], headers = {})
    Rack::Cache::Response.new(200,
      { 'cache-control' => 'max-age=420' }.merge(headers), body)
  end

  def store(body_text = 'test', request_or_uri = nil)
    request =
      case request_or_uri
      when nil then mock_request
      when String then mock_request(request_or_uri)
      else request_or_uri
      end
    response = mock_response([body_text])
    @meta.store(request, response, @entities)
    response.headers['x-content-digest']
  end

  def slurp(body)
    buf = ''
    body.each { |part| buf << part }
    buf
  end

  # MetaStore that fails its low level metadata #write a controlled number
  # of times, so rollback behavior is observable.
  class FailingMetaStore < Rack::Cache::MetaStore::Heap
    attr_accessor :fail_writes, :write_calls

    def initialize(*)
      super
      @fail_writes = 0
      @write_calls = 0
    end

    def write(key, entries, ttl = nil)
      @write_calls += 1
      if @fail_writes > 0
        @fail_writes -= 1
        raise 'simulated metadata write failure'
      end
      super
    end
  end

  # EntityStore that fails #write once.
  class FailingEntityStore < Rack::Cache::EntityStore::Heap
    attr_accessor :fail_writes

    def write(body, ttl = nil)
      if @fail_writes.to_i > 0
        @fail_writes -= 1
        raise 'simulated entity write failure'
      end
      super
    end
  end

  # ---- References ----------------------------------------------------------

  it 'reports the Heap backend as reference managed and others as not' do
    assert @entities.reference_managed?
    refute Rack::Cache::EntityStore::Disk.
      new(File.join(Dir.tmpdir, "rc-ref-#{$$}-#{object_id}")).reference_managed?
    refute Rack::Cache::EntityStore.new.reference_managed?
  end

  it 'counts one body reference per stored entry' do
    digest = store
    @entities.refcount(digest).must_equal 1
    assert @entities.exist?(digest)
  end

  it 'shares one body and one reference count across identical entries' do
    request_a = mock_request('http://example.com/a')
    request_b = mock_request('http://example.com/b')
    first_response = mock_response(['shared'])
    @meta.store(request_a, first_response, @entities)
    digest1 = first_response.headers['x-content-digest']

    # second, separate response object carrying the same body
    second_response = mock_response(['shared'])
    @meta.store(request_b, second_response, @entities)
    digest2 = second_response.headers['x-content-digest']

    digest1.must_equal digest2
    @entities.refcount(digest1).must_equal 2

    @entities.read(digest1).must_equal 'shared'
  end

  it 'keeps the body when one of two sharing entries is evicted' do
    digest = store('shared', 'http://example.com/a')
    store('shared', 'http://example.com/b')
    @entities.refcount(digest).must_equal 2

    key_a = Rack::Cache::Key.call(mock_request('http://example.com/a'))
    @meta.send(:remove_entry, key_a, @entities)

    @entities.refcount(digest).must_equal 1
    assert @entities.exist?(digest)
    @entities.read(digest).must_equal 'shared'
  end

  it 'reclaims the body only when the last referencing entry is removed' do
    digest = store('shared', 'http://example.com/a')
    store('shared', 'http://example.com/b')

    key_a = Rack::Cache::Key.call(mock_request('http://example.com/a'))
    key_b = Rack::Cache::Key.call(mock_request('http://example.com/b'))

    reclaimed_a = @meta.send(:remove_entry, key_a, @entities)
    refute_nil digest
    assert @entities.exist?(digest),
      'body must survive while another entry references it'
    reclaimed_a.must_be_nil

    @meta.send(:remove_entry, key_b, @entities)
    refute @entities.exist?(digest),
      'body must be reclaimed when the last entry goes away'
    @entities.refcount(digest).must_equal 0
    @entities.read(digest).must_be_nil
  end

  # ---- Invalidation is not deletion ----------------------------------------

  it 'does not reclaim a body on invalidation: the entry only goes stale' do
    request = mock_request
    digest = store('keep me', request)

    @meta.invalidate(request, @entities)

    @entities.refcount(digest).must_equal 1
    assert @entities.exist?(digest)

    response = @meta.lookup(request, @entities)
    refute response.fresh?
    slurp(response.body).must_equal 'keep me'
  end

  # ---- Replacement ---------------------------------------------------------

  it 'does not reclaim the shared body on same-digest replacement' do
    request = mock_request
    digest = store('same body', request)

    replacement = mock_response(['same body'])
    @meta.store(request, replacement, @entities)

    @entities.refcount(digest).must_equal 1
    assert @entities.exist?(digest)
    slurp(@meta.lookup(request, @entities).body).must_equal 'same body'
  end

  it 'reclaims the displaced body when an entry is replaced by a new one' do
    request = mock_request
    old_digest = store('old body', request)

    replacement = mock_response(['brand new body'])
    @meta.store(request, replacement, @entities)
    new_digest = replacement.headers['x-content-digest']

    new_digest.wont_equal old_digest
    refute @entities.exist?(old_digest),
      'the body the replaced entry pointed at must be reclaimed'
    assert @entities.exist?(new_digest)
    @entities.refcount(new_digest).must_equal 1
  end

  # ---- Metadata write failure ---------------------------------------------

  it 'rolls back references and purges the orphan body when meta write fails' do
    meta = FailingMetaStore.new
    meta.fail_writes = 1

    request = mock_request
    response = mock_response(['doomed'])
    assert_raises(RuntimeError) do
      meta.store(request, response, @entities)
    end

    digest = response.headers['x-content-digest']
    refute_nil digest
    refute @entities.exist?(digest),
      'body written for a failed metadata write must be purged'
    @entities.refcount(digest).must_equal 0
    meta.read(Rack::Cache::Key.call(request)).must_equal []

    # the store remains usable after the failed write
    retry_response = mock_response(['doomed'])
    meta.store(request, retry_response, @entities)
    retry_digest = retry_response.headers['x-content-digest']
    @entities.refcount(retry_digest).must_equal 1
    assert @entities.exist?(retry_digest)
  end

  it 'does not release a shared body when a failing write is rolled back' do
    # two keys share one body in the same reference managed meta store
    request_a = mock_request('http://example.com/a')
    request_b = mock_request('http://example.com/b')
    response_a = mock_response(['shared body'])
    @meta.store(request_a, response_a, @entities)
    shared = response_a.headers['x-content-digest']
    response_b = mock_response(['shared body'])
    @meta.store(request_b, response_b, @entities)
    response_b.headers['x-content-digest'].must_equal shared
    @entities.refcount(shared).must_equal 2

    meta = FailingMetaStore.new(@meta.to_hash)
    meta.fail_writes = 1

    # replacing key A references a *new* digest; the write fails while
    # committing metadata, so the displaced shared reference must be restored
    assert_raises(RuntimeError) do
      meta.store(request_a, mock_response(['replacement']), @entities)
    end

    assert @entities.exist?(shared),
      'the body shared with entry B must not be reclaimed on rollback'
    @entities.refcount(shared).must_equal 2

    # key B still resolves and reads the shared body
    slurp(@meta.lookup(request_b, @entities).body).must_equal 'shared body'
  end

  it 'leaves no reference behind when the entity write itself raises' do
    entities = FailingEntityStore.new
    entities.fail_writes = 1
    request = mock_request
    assert_raises(RuntimeError) do
      @meta.store(request, mock_response(['x']), entities)
    end
    # the entity never landed, so there is nothing to reference; the meta
    # store must be empty and a retry works cleanly
    response = mock_response(['x'])
    @meta.store(request, response, entities)
    digest = response.headers['x-content-digest']
    @meta.read(Rack::Cache::Key.call(request)).length.must_equal 1
    entities.refcount(digest).must_equal 1
  end

  # ---- Reads in flight -----------------------------------------------------

  it 'keeps a body readable after the last entry is evicted mid-read' do
    request = mock_request
    digest = store('read me to the end', request)

    body = @entities.open(digest)
    @entities.readers(digest).must_equal 1

    key = Rack::Cache::Key.call(request)
    @meta.send(:remove_entry, key, @entities)

    # the last reference is gone but a reader is still iterating the body:
    # reclamation is deferred until the reader finishes/closes, yet the data
    # remains readable in the meantime.
    @entities.refcount(digest).must_equal 0
    assert @entities.exist?(digest),
      'body stays available while a handed-out read is in flight'
    @entities.readers(digest).must_equal 1

    slurp(body).must_equal 'read me to the end'
    body.close
    @entities.readers(digest).must_equal 0
    refute @entities.exist?(digest),
      'last reader closing reclaims the unreferenced body'
  end

  it 'serves a lookup body fully even when the entry is evicted concurrently' do
    request = mock_request
    digest = store('concurrent read', request)

    response = @meta.lookup(request, @entities)
    @entities.readers(digest).must_equal 1

    @meta.send(:remove_entry, Rack::Cache::Key.call(request), @entities)
    slurp(response.body).must_equal 'concurrent read'
    response.body.close
  end

  it 'closes reader pins exactly once' do
    digest, = @entities.write(['pin'])
    @entities.retain(digest)
    body = @entities.open(digest)
    @entities.readers(digest).must_equal 1
    body.close
    body.close # idempotent
    @entities.readers(digest).must_equal 0
    # releasing the single reference reclaims the now unpinned body
    @entities.release(digest).must_equal true
    refute @entities.exist?(digest)
  end

  it 'releases a reader pin via garbage collection when never closed' do
    skip 'finalizers on MRI only' unless RUBY_ENGINE == 'ruby'
    digest, = @entities.write(['gc'])
    @entities.retain(digest)
    # The Body returned here is intentionally abandoned without #close;
    # its finalizer must release the reader pin so the body can be reclaimed.
    proc { @entities.open(digest) }.call
    10.times { GC.start; (GC.compact rescue nil) }
    @entities.readers(digest).must_equal 0
    # dropping the last meta reference now reclaims the unpinned body
    @entities.release(digest).must_equal true
    refute @entities.exist?(digest)
  end

  it 'handles concurrent stores, lookups and evictions without corruption' do
    threads = 8
    iterations = 60
    errors = []
    bad_reads = []
    barrier = Queue.new

    workers = Array.new(threads) do |tid|
      Thread.new do
        barrier.pop
        iterations.times do |i|
          begin
            text = "body-#{i % 5}" # several threads share identical bodies
            uri = "http://example.com/#{tid}/#{i % 7}"
            request = mock_request(uri)
            response = mock_response([text])
            @meta.store(request, response, @entities)
            digest = response.headers['x-content-digest']

            hit = @meta.lookup(mock_request(uri), @entities)
            if hit
              content = slurp(hit.body)
              bad_reads << [uri, content] unless content == text
              hit.body.close
            end

            if i % 3 == 0
              @meta.send(:remove_entry,
                Rack::Cache::Key.call(request), @entities)
            end
          rescue Exception => error
            errors << error
          end
        end
      end
    end

    threads.times { barrier << :go }
    workers.each(&:join)
    errors.must_equal []
    bad_reads.must_equal []

    # invariant: every surviving body has at least one meta reference, and
    # every referenced digest exists in the entity store
    referenced = Hash.new(0)
    @meta.to_hash.each_value do |dump|
      Marshal.load(dump).each do |_, res|
        referenced[res['x-content-digest']] += 1 if res['x-content-digest']
      end
    end

    referenced.each do |digest, count|
      @entities.refcount(digest).must_equal count
      assert count > 0
      assert @entities.exist?(digest),
        "referenced body #{digest} must exist (no dangling references)"
    end

    @entities.instance_variable_get(:@hash).each_key do |digest|
      assert referenced[digest] > 0,
        "unreferenced body #{digest} was not reclaimed"
    end
  end

  it 'does not create a reference for a missing body (no dangling refs)' do
    @entities.retain('deadbeef' * 5).must_equal false
    @entities.refcount('deadbeef' * 5).must_equal 0
  end

  it 'reclaims a body whose references all fail to commit' do
    digest, = @entities.write(['lonely'])
    @entities.retain(digest).must_equal true
    @entities.refcount(digest).must_equal 1
    @entities.release(digest).must_equal true
    refute @entities.exist?(digest)
  end

  it 'does not reclaim anything when invalidation has nothing to write' do
    request = mock_request
    # already-stale response (expired): invalidate leaves entries untouched
    response = mock_response(['x'],
      'expires' => (Time.now - 3600).httpdate)
    @meta.store(request, response, @entities)
    digest = response.headers['x-content-digest']

    @meta.invalidate(request, @entities)

    @entities.refcount(digest).must_equal 1
    assert @entities.exist?(digest)
  end

  it 'reclaims after a dangling lookup purges the meta entry' do
    request = mock_request
    digest = store('soon dangling', request)

    # simulate an entity lost out-of-band (e.g. external purge)
    @entities.purge(digest)

    @meta.lookup(request, @entities).must_be_nil
    @meta.read(Rack::Cache::Key.call(request)).must_equal []
  end
end
