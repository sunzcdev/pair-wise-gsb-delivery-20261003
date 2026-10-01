$LOAD_PATH.unshift 'lib'
require 'rack/cache'
require 'rack/cache/storage'

storage = Rack::Cache::Storage.new
bodies = { '/file' => '0123456789' }
app = lambda do |env|
  full = bodies[Rack::Request.new(env).path]
  h = { 'content-type'=>'application/octet-stream','etag'=>'"v1"','cache-control'=>'max-age=60','accept-ranges'=>'bytes' }
  if r = env['HTTP_RANGE']
    m = /bytes=(\d+)-(\d*)/.match(r)
    first = m[1].to_i
    last = m[2].empty? ? full.bytesize-1 : m[2].to_i
    data = full.byteslice(first, last-first+1)
    h['content-range'] = "bytes #{first}-#{last}/#{full.bytesize}"
    h['content-length'] = data.bytesize.to_s
    [206,h,[data]]
  else
    h['cache-control'] = 'private'
    h['content-length'] = full.bytesize.to_s
    [200,h,[full]]
  end
end
cache = Rack::Cache::Context.new(app) { |c| c.set 'rack-cache.fragment_caching', true; c.set 'rack-cache.storage', storage }

def ask(cache, storage, path, opts={})
  env = Rack::MockRequest.env_for(path, {'rack.run_once'=>true,'rack.errors'=>$stderr}.merge(opts))
  status, headers, body = cache.call(env)
  data = body.join
  puts "#{opts['HTTP_RANGE'] ? 'RANGE '+opts['HTTP_RANGE'] : 'GET  '+path} -> #{status} trace=#{cache.trace.inspect} body=#{data.inspect} cl=#{headers['content-length']} cr=#{headers['content-range']}"
end
ask(cache, storage, '/file', 'HTTP_RANGE'=>'bytes=0-2')
ask(cache, storage, '/file')
ask(cache, storage, '/file', 'HTTP_RANGE'=>'bytes=6-9')
ask(cache, storage, '/file', 'HTTP_RANGE'=>'bytes=0-2')
ask(cache, storage, '/file', 'HTTP_RANGE'=>'bytes=3-5')
ask(cache, storage, '/file')
