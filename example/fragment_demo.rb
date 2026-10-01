$LOAD_PATH.unshift File.expand_path('../lib', __dir__ || '.')
require 'rack/cache'
require 'stringio'

ORIGINAL = '0123456789AB'.b # 12 bytes

def origin_for(etag)
  lambda do |env|
    headers = {
      'etag' => etag,
      'cache-control' => 'max-age=300',
      'content-type' => 'application/octet-stream',
      'accept-ranges' => 'bytes'
    }
    if (range = env['HTTP_RANGE']) && range =~ /\Abytes=(\d+)-(\d+)\z/
      s, f = $1.to_i, $2.to_i
      [206, headers.merge('content-range' => "bytes #{s}-#{f}/#{ORIGINAL.bytesize}"),
       [ORIGINAL.byteslice(s, f - s + 1)]]
    else
      [200, headers, [ORIGINAL]]
    end
  end
end

def new_cache(backend)
  storage = Rack::Cache::Storage.new
  cache = Rack::Cache::Context.new(backend) do |c|
    c.set 'rack-cache.store_fragments' => true
  end
  [cache, storage]
end

def ask(cache, storage, label, env = {})
  status, headers, body =
    cache.call(Rack::MockRequest.env_for('/file', env).merge(
      'rack.run_once' => true,
      'rack.errors' => StringIO.new,
      'rack-cache.storage' => storage))
  buf = +''
  body.each { |p| buf << p }
  puts "  %-30s -> %s  trace=%s  body=%p" %
    [label, status, headers['x-rack-cache'], buf]
  [status, headers, buf]
end

def published?(cache, storage)
  env = Rack::MockRequest.env_for('/file').merge(
    'rack-cache.storage' => storage)
  !cache.metastore.lookup(
    Rack::Cache::Request.new(env), cache.entitystore).nil?
end

puts '== 1. out-of-order fragments; promotion at the covering byte =='
cache, storage = new_cache(origin_for('"v1"'))
ask(cache, storage, 'Range bytes=6-11', 'HTTP_RANGE' => 'bytes=6-11')
ask(cache, storage, 'Range bytes=0-2',  'HTTP_RANGE' => 'bytes=0-2')
puts '  complete entry published? ' + published?(cache, storage).to_s
ask(cache, storage, 'Range bytes=3-5',  'HTTP_RANGE' => 'bytes=3-5')
puts '  complete entry published? ' + published?(cache, storage).to_s
ask(cache, storage, 'plain GET (fresh)')

puts
puts '== 2. failing writes never publish, good fragments survive =='
bad = lambda do |env|
  if env['HTTP_RANGE'] == 'bytes=3-5'
    [206, { 'etag' => '"v1"', 'cache-control' => 'max-age=300',
            'content-type' => 'application/octet-stream',
            'content-range' => 'bytes 3-5/12' }, ['ZZZ']]
  else
    origin_for('"v1"').call(env)
  end
end
cache, storage = new_cache(bad)
ask(cache, storage, 'Range bytes=0-4',   'HTTP_RANGE' => 'bytes=0-4')
ask(cache, storage, 'Range 3-5 CONFLICT', 'HTTP_RANGE' => 'bytes=3-5')
puts '  complete entry published? ' + published?(cache, storage).to_s
# a correct replacement fragment completes the assembly
good_tail = lambda do |env|
  origin_for('"v1"').call(env)
end
cache, storage2 = new_cache(good_tail)
ask(cache, storage2, 'Range bytes=0-4', 'HTTP_RANGE' => 'bytes=0-4')
ask(cache, storage2, 'Range bytes=5-11', 'HTTP_RANGE' => 'bytes=5-11')
puts '  complete entry published? ' + published?(cache, storage2).to_s

puts
puts '== 3. validator rotation isolates versions =='
cache, storage = new_cache(origin_for('"v1"'))
ask(cache, storage, 'Range bytes=0-4 (v1)', 'HTTP_RANGE' => 'bytes=0-4')
puts '  representations tracked: ' + cache.fragment_store.size.to_s

# origin regenerates the resource: new strong validator for the same URI
cache2, storage3 = new_cache(origin_for('"v2"'))
# share the in-memory fragment assembly so the rotation is visible
cache2.instance_variable_set(:@fragment_store, cache.fragment_store)
ask(cache2, storage3, 'Range bytes=5-9 (v2)', 'HTTP_RANGE' => 'bytes=5-9')
puts '  representations tracked: ' + cache.fragment_store.size.to_s
puts '  (the v1 and v2 tails never merge; each must finish independently)'
ask(cache2, storage3, 'Range bytes=0-4 (v2)', 'HTTP_RANGE' => 'bytes=0-4')
ask(cache2, storage3, 'Range bytes=10-11 (v2)', 'HTTP_RANGE' => 'bytes=10-11')
puts '  v2 complete entry published? ' + published?(cache2, storage3).to_s
ask(cache2, storage3, 'plain GET (fresh v2)')
