require_relative 'test_helper'

require 'sinatra/param_sources'
require 'json'

describe 'Sinatra::ParamSources' do
  def register_app(&block)
    mock_app do
      register Sinatra::ParamSources
      class_exec(&block)
    end
  end

  describe 'source views' do
    before do
      register_app do
        get('/users/:id') do
          content_type 'text/plain'
          JSON.dump(
            params: params,
            route: route_params,
            query: query_params,
            body: body_params,
            sources: param_source_of(:id),
            occurrences: param_occurrences(:id),
            raw_query: raw_param_pairs(:query)
          )
        end
      end
    end

    it 'keeps route, query and body trees separate' do
      get '/users/7?id=9&extra=b'
      assert_equal 200, response.status

      data = JSON.parse(body)
      assert_equal({ 'id' => '7' }, data['route'])
      assert_equal({ 'id' => '9', 'extra' => 'b' }, data['query'])
      assert_equal({}, data['body'])
      assert_equal %w[route query], data['sources']
    end

    it 'populates the body tree for form requests' do
      register_app do
        post('/users/:id') do
          content_type 'text/plain'
          JSON.dump(route: route_params, query: query_params, body: body_params)
        end
      end

      post '/users/7?id=9', id: 8, extra: 'b'
      assert_equal 200, response.status
      data = JSON.parse(body)
      assert_equal({ 'id' => '7' }, data['route'])
      assert_equal({ 'id' => '9' }, data['query'])
      assert_equal({ 'id' => '8', 'extra' => 'b' }, data['body'])
    end

    it 'preserves nested parameter trees per source' do
      register_app do
        post('/') do
          content_type 'text/plain'
          JSON.dump(q: query_params['f'], b: body_params['f'])
        end
      end

      post '/?f[name]=q', { 'f' => { 'name' => 'b', 'age' => '3' } }
      data = JSON.parse(body)
      assert_equal({ 'name' => 'q' }, data['q'])
      assert_equal({ 'name' => 'b', 'age' => '3' }, data['b'])
    end

    it 'remembers repeated plain query keys through occurrences' do
      get '/users/7?id=9&id=10'
      data = JSON.parse(body)
      assert_equal '7', data['params']['id']
      assert_equal '9', data['raw_query'].first.last
      ids = data['occurrences'].select { |entry| entry['source'] == 'query' }
      assert_equal %w[9 10], ids.map { |entry| entry['value'] }
      assert_includes data['occurrences'].map { |entry| entry['source'] }, 'route'
    end

    it 'resolves nested and bracketed key paths to their source' do
      register_app do
        get('/u/:id') do
          content_type 'text/plain'
          JSON.dump(
            nested: param_occurrences('f[name]'),
            array: param_occurrences('a[]'),
            from_query: param_from?(:query, :'f[name]')
          )
        end
      end

      get '/u/7?f[name]=bob&a[]=1&a[]=2'
      data = JSON.parse(body)
      assert_equal 1, data['nested'].size
      assert_equal 'f[name]', data['nested'].first['key']
      assert_equal %w[1 2], data['array'].map { |entry| entry['value'] }
      assert_equal true, data['from_query']
    end
  end

  describe 'allowed sources' do
    before do
      register_app do
        param_source :id, :route
        param_source :only_query, :query
        param_source :either, :query, :body
        post('/users/:id') { 'ok' }
        get('/users/:id') { 'ok' }
      end
    end

    it 'accepts the declared source' do
      get '/users/7'
      assert_equal 200, response.status
    end

    it 'rejects query overrides for a route-only param before the block' do
      executed = false
      register_app do
        param_source :id, :route
        get('/users/:id') { executed = true; 'ok' }
      end

      get '/users/7?id=9'
      assert_equal 400, response.status
      assert_equal false, executed
    end

    it 'rejects body values for a route-only param' do
      post '/users/7', id: 9
      assert_equal 400, response.status
    end

    it 'rejects a route capture for a query-only param' do
      register_app do
        param_source :only_query, :query
        get('/u/:only_query') { 'ok' }
      end

      get '/u/value'
      assert_equal 400, response.status
    end

    it 'accepts any of the listed sources' do
      get '/users/7?either=q'
      assert_equal 200, response.status
      post '/users/7', { either: 'b' }
      assert_equal 200, response.status
    end

    it 'does not reject when the parameter is absent' do
      get '/users/7'
      assert_equal 200, response.status
    end
  end

  describe 'conflict policies' do
    it 'rejects conflicts by default' do
      register_app do
        param_source :tag, :query, :body
        post('/items/:n') { 'ok' }
      end

      post '/items/1?tag=q', tag: 'b'
      assert_equal 400, response.status
    end

    it 'allows conflicts when on_conflict is allow' do
      register_app do
        param_source :tag, :query, :body, on_conflict: :allow
        post('/items/:n') { 'ok' }
      end

      post '/items/1?tag=q', tag: 'b'
      assert_equal 200, response.status
    end

    it 'detects scalar versus array conflicts under prefer' do
      register_app do
        param_source :tag, :query, :body, on_conflict: :prefer
        post('/items/:n') { 'ok' }
      end

      post '/items/1?tag=q', 'tag[]=b',
           { 'CONTENT_TYPE' => 'application/x-www-form-urlencoded' }
      assert_equal 400, response.status

      post '/items/1?tag[]=q', 'tag=b',
           { 'CONTENT_TYPE' => 'application/x-www-form-urlencoded' }
      assert_equal 400, response.status
    end

    it 'allows same-shape conflicts under prefer' do
      register_app do
        param_source :tag, :query, :body, on_conflict: :prefer
        post('/items/:n') { 'ok' }
      end

      post '/items/1?tag=q', tag: 'b'
      assert_equal 200, response.status
    end

    it 'applies the policy to nested parameters' do
      register_app do
        param_source 'f[name]', :query
        post('/') { 'ok' }
        get('/') { 'ok' }
      end

      get '/?f[name]=x'
      assert_equal 200, response.status

      post '/', 'f[name]=x', { 'CONTENT_TYPE' => 'application/x-www-form-urlencoded' }
      assert_equal 400, response.status
    end
  end

  describe 'route passes' do
    it 'does not carry route captures over to the next candidate' do
      register_app do
        get('/things/:x') do
          pass if params[:x] == 'skip'
          "first:#{route_params[:x]}"
        end

        get('/things/skip') do
          "second:#{route_params[:x].inspect}:#{params[:x].inspect}"
        end
      end

      get '/things/skip?x=q'
      assert_equal 200, response.status
      assert_equal 'second:nil:nil', body
    end

    it 'enforces the freshly captured route on the next candidate' do
      register_app do
        param_source :x, :route

        get('/things/:x') { pass; 'first' }
        get('/things/keep') { 'second' }
      end

      get '/things/keep'
      assert_equal 200, response.status
      assert_equal 'second', body
    end

    it 'skips enforcement when a route condition fails and passes' do
      register_app do
        param_source :id, :route
        get('/users/:id', provides: :json) { 'ok' }
      end

      get '/users/7?id=9', {}, 'HTTP_ACCEPT' => 'text/html'
      assert_equal 404, response.status
    end
  end

  describe 'shapes and raw paths' do
    it 'exposes route captures such as splat arrays' do
      register_app do
        get('/a/*/b/*') do
          content_type 'text/plain'
          JSON.dump(splat: route_params['splat'],
                    sources: param_source_of('splat'),
                    paths: raw_param_paths(:route))
        end
      end

      get '/a/x/b/y'
      data = JSON.parse(body)
      assert_equal %w[x y], data['splat']
      assert_equal ['route'], data['sources']
      assert_includes data['paths'], ['splat']
    end

    it 'keeps multipart form fields in the body source' do
      boundary = 'AaB03x'
      payload = [
        "--#{boundary}",
        'content-disposition: form-data; name="title"',
        '',
        'hello',
        "--#{boundary}--",
        ''
      ].join("\r\n")

      register_app do
        post('/upload') do
          content_type 'text/plain'
          JSON.dump(body: body_params, keys: raw_param_pairs(:body).map(&:first))
        end
      end

      post '/upload', payload,
           'CONTENT_TYPE' => "multipart/form-data; boundary=#{boundary}"
      assert_equal 200, response.status
      data = JSON.parse(body)
      assert_equal({ 'title' => 'hello' }, data['body'])
      assert_equal ['title'], data['keys']
    end

    it 'does not parse JSON request bodies as parameters' do
      register_app do
        post('/items') do
          content_type 'text/plain'
          body_params.keys.inspect
        end
      end

      post '/items', '{"id":5}', 'CONTENT_TYPE' => 'application/json'
      assert_equal 200, response.status
      assert_equal '[]', body
    end

    it 'restores route sources inside error handlers' do
      register_app do
        param_source :id, :route

        get('/boom/:id') { raise 'boom' }

        error StandardError do
          status 500
          "handled:#{route_params[:id]}:#{params[:id]}"
        end
      end

      get '/boom/7'
      assert_equal 500, response.status
      assert_equal 'handled:7:7', body
    end
  end

  describe 'default behavior' do
    it 'keeps the merged params behavior when no rules are declared' do
      mock_app do
        register Sinatra::ParamSources
        get('/users/:id') { "#{params[:id]}:#{query_params[:id]}" }
      end

      get '/users/7?id=9'
      assert_equal 200, response.status
      assert_equal '7:9', body
    end

    it 'stays a no-op when the extension is not registered' do
      mock_app do
        get('/users/:id') { params[:id] }
      end

      get '/users/7?id=9'
      assert_equal 200, response.status
      assert_equal '7', body
    end
  end

  describe 'inheritance' do
    it 'copies rules into subclasses without sharing them' do
      parent = Class.new(Sinatra::Base) do
        set :environment, :test
        register Sinatra::ParamSources
        param_source :id, :route
      end

      child = Class.new(parent)
      child.param_source :other, :query

      assert_nil parent.param_source_rules[:other]
      refute_nil child.param_source_rules[:id]
    end
  end
end
