require_relative 'test_helper'
require 'rack/cache/meta_store'
require 'rack/cache/entity_store'

# These tests prove that the paired Heap MetaStore/EntityStore keep body
# references consistent through entry creation, replacement, invalidation
# and deletion, including metadata write failures and concurrent readers.
describe 'Heap body reference management' do
  FailingWriteError = Class.new(StandardError)

  # Heap MetaStore whose low level #write fails on demand, providing a
  # controllable failure point between body write and meta commit.
  class FailingMetaStore < Rack::Cache::MetaStore::Heap
    attr_accessor :fail_writes

    def write(*)
      raise FailingWriteError, 'metadata write failed' if @fail_writes
      super
    end
  end

  before do
    @meta = Rack::Cache::MetaStore::Heap.new
    @entity = Rack::Cache::EntityStore::Heap.new
  end

  def mock_request(path='/test', headers={})
    Rack::Cache::Request.new(Rack::MockRequest.env_for(path, headers))
  end

  def store_entry(path='/test', body=['test'], response_headers={}, meta=@meta)
    request = mock_request(path)
    headers = { 'cache-control' => 'max-age=420' }.merge(response_headers)
    response = Rack::Cache::Response.new(200, headers, body)
    meta.store(request, response, @entity)
    digest = response.headers['x-content-digest']
    # #store hands an opened body back to the caller; release that reader
    # pin so the tests observe reference (not reader) driven reclamation.
    response.body.close
    digest
  end

  def body_references
    @entity.instance_variable_get(:@references)
  end

  def body_readers
    @entity.instance_variable_get(:@readers)
  end

  it 'references a stored body exactly once per stored entry' do
    digest = store_entry
    assert @entity.exist?(digest)
    body_references[digest].must_equal 1
  end

  it 'shares one body between identical entries and keeps it until the last goes' do
    digest_a = store_entry('/a', ['same'])
    digest_b = store_entry('/b', ['same'])
    digest_a.must_equal digest_b
    body_references[digest_a].must_equal 2

    # replace the entry at /a with different content: /b still pins the body
    new_digest = store_entry('/a', ['different'])
    refute new_digest == digest_a
    assert @entity.exist?(digest_a), 'shared body must survive while /b references it'
    body_references[digest_a].must_equal 1
    assert @entity.exist?(new_digest)

    # removing the final referencing entry reclaims the body
    key_b = @meta.cache_key(mock_request('/b'))
    @meta.purge(key_b, @entity)
    refute @entity.exist?(digest_a), 'body reclaimed when the final entry goes'
  end

  it 'replaces an entry body and reclaims the previous one at commit' do
    old_digest = store_entry('/doc', ['version one'])
    new_digest = store_entry('/doc', ['version two'])
    refute old_digest == new_digest
    refute @entity.exist?(old_digest), 'evicted body must be reclaimed'
    assert @entity.exist?(new_digest)
    body_references[new_digest].must_equal 1
  end

  it 'does not touch the body when an entry is replaced with identical content' do
    digest = store_entry('/doc', ['identical'])
    backing = @entity.instance_variable_get(:@hash)
    stored_body = backing[digest]

    store_entry('/doc', ['identical'])

    body_references[digest].must_equal 1
    assert @entity.exist?(digest)
    assert_same stored_body, backing[digest],
      'same-digest replacement must not rewrite or reclaim the body'
  end

  it 'keeps invalidated (stale) entries and their bodies' do
    digest = store_entry('/doc', ['stale soon'])
    @meta.invalidate(mock_request('/doc'), @entity)

    assert @entity.exist?(digest), 'invalidation is not deletion'
    body_references[digest].must_equal 1

    response = @meta.lookup(mock_request('/doc'), @entity)
    response.body.close
    refute response.nil?
    refute response.fresh?
  end

  it 'rolls back references when the metadata write raises, leaving no orphan body' do
    failing = FailingMetaStore.new
    failing.fail_writes = true
    request = mock_request('/doc')
    response = Rack::Cache::Response.new(200,
      { 'cache-control' => 'max-age=420' }, ['fresh data'])

    assert_raises(FailingWriteError) do
      failing.store(request, response, @entity)
    end

    digest = response.headers['x-content-digest']
    # The handed-out body stays fully readable despite the failed commit...
    delivered = response.body.to_a.join
    delivered.must_equal 'fresh data'
    body_readers[digest].must_equal 1

    # ...and is reclaimed once that last reader goes away.
    response.body.close
    refute @entity.exist?(digest)
    assert body_references.empty?, 'no dangling references after failure'
    assert body_readers.empty?
  end

  it 'preserves the old entry and its body when a replacement write fails' do
    failing = FailingMetaStore.new
    old_digest = store_entry('/doc', ['old body'], {}, failing)

    failing.fail_writes = true
    request = mock_request('/doc')
    replacement = Rack::Cache::Response.new(200,
      { 'cache-control' => 'max-age=420' }, ['new body'])
    new_digest = replacement.headers['x-content-digest']
    assert_raises(FailingWriteError) do
      failing.store(request, replacement, @entity)
    end
    replacement.body.close

    assert @entity.exist?(old_digest), 'committed body must survive failed rewrite'
    body_references[old_digest].must_equal 1
    refute @entity.exist?(new_digest), 'tentative new body must be reclaimed'

    served = failing.lookup(mock_request('/doc'), @entity)
    served.body.to_a.join.must_equal 'old body'
    served.body.close
  end

  it 'pins a body handed to a reader past deletion and reclaims it on close' do
    digest = store_entry('/doc', ['streaming'])
    served = @meta.lookup(mock_request('/doc'), @entity)

    # The last entry disappears while the body is being read.
    key = @meta.cache_key(mock_request('/doc'))
    @meta.purge(key, @entity)

    assert @entity.exist?(digest), 'in-progress reader keeps the body alive'
    body_readers[digest].must_equal 1
    served.body.to_a.join.must_equal 'streaming'

    served.body.close
    refute @entity.exist?(digest), 'reclamation happens when the last reader closes'
    assert body_readers.empty?
  end

  it 'waits for every concurrent reader before reclaiming' do
    digest = store_entry('/doc', ['shared stream'])
    reader_one = @entity.open(digest)
    reader_two = @entity.open(digest)
    body_readers[digest].must_equal 2

    key = @meta.cache_key(mock_request('/doc'))
    @meta.purge(key, @entity)
    refute body_references.key?(digest)

    reader_one.to_a.join.must_equal 'shared stream'
    reader_one.close
    assert @entity.exist?(digest), 'one reader remains'
    reader_two.to_a.join.must_equal 'shared stream'
    reader_two.close
    refute @entity.exist?(digest), 'reclaimed after final reader close'
  end

  it 'purges dangling meta entries discovered at lookup and releases the key' do
    digest = store_entry('/doc', ['ghost'])
    @entity.purge(digest)
    result = @meta.lookup(mock_request('/doc'), @entity)
    result.must_be_nil
    assert @meta.read(@meta.cache_key(mock_request('/doc'))).empty?
  end

  it 'keeps an open body readable after a force purge and tolerates close' do
    digest = store_entry('/doc', ['force'])
    body = @entity.open(digest)
    @entity.purge(digest)
    body.to_a.join.must_equal 'force'
    body.close
    body.close # closing twice is a no-op
    assert body_readers.empty?
  end
end