require_relative 'test/test_helper'
include CacheContextHelpers
setup_cache_context
@bodies = { '/file' => '0123456789' }
@etags = { '/file' => '"v1"' }
@poison_ranges = []
@truncate_ranges = []
@app = lambda do |env|
  full = @bodies[Rack::Request.new(env).path]
  h = { 'content-type'=>'application/octet-stream','etag'=>'"v1"','cache-control'=>'max-age=60','accept-ranges'=>'bytes' }
  if r = env['HTTP_RANGE']
    m = /bytes=(\d+)-(\d*)/.match(r)
    first=m[1].to_i; last = m[2].empty? ? full.bytesize-1 : m[2].to_i
    data = full.byteslice(first,last-first+1)
    h['content-range']="bytes #{first}-#{last}/#{full.bytesize}"; h['content-length']=data.bytesize.to_s
    [206,h,[data]]
  else
    h['cache-control']='private'; h['content-length']=full.bytesize.to_s
    [200,h,[full]]
  end
end
@cache_config = proc { |c| c.set 'rack-cache.fragment_caching', true }
r = request(:get, '/file', 'HTTP_RANGE'=>'bytes=0-2')
puts "first: #{r.status} trace=#{@caches.last.trace.inspect}"
r = get '/file'
puts "plain: #{r.status} trace=#{@caches.last.trace.inspect} body=#{r.body.inspect} keys=#{r.headers.keys.inspect}"
