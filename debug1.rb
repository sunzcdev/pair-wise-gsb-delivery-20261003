$LOAD_PATH.unshift 'lib'
require 'rack/cache'
require 'rack/cache/storage'
store = Rack::Cache::FragmentStore.new
meta = Rack::Cache::MetaStore::Heap.new
ent = Rack::Cache::EntityStore::Heap.new
env = Rack::MockRequest.env_for('/file'); env['rack-cache.cache_key'] = Rack::Cache::Key
req = Rack::Cache::Request.new(env)
def fr(first,last,total,data,h={})
  h = {'etag'=>'"v1"','content-type'=>'application/octet-stream','content-range'=>"bytes #{first}-#{last}/#{total}",'content-length'=>(last-first+1).to_s,'cache-control'=>'max-age=60'}.merge(h)
  Rack::Cache::Response.new(206,h,[data])
end
p store.store(req, fr(0,4,8,'01234'), meta, ent)
begin
  p store.store(req, fr(3,7,8,'XXXX567'), meta, ent)
rescue => e
  puts "first error: #{e.message}"
end
begin
  p store.store(req, fr(5,7,8,'567'), meta, ent)
rescue => e
  puts "second error: #{e.message}"
end
p store.size

