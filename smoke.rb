require 'rack/cache'
puts Rack::Cache::FragmentStore
p Rack::Cache::FragmentStore.parse_content_range('bytes 0-99/500')
p Rack::Cache::FragmentStore.parse_content_range('bytes 0-499/*')
p Rack::Cache::FragmentStore.strong_etag?('W/"x"')
p Rack::Cache::FragmentStore.strong_etag?('"x"')
p Rack::Cache::FragmentStore.strong_etag?('')
