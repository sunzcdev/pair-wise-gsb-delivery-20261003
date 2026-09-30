require_relative 'test_helper'

class ParamSourcesTest < Minitest::Test
  it 'keeps route, query and body params in separate trees' do
    mock_app do
      post('/users/:id') do
        [
          param_sources[:route]['id'],
          param_sources[:query]['id'],
          param_sources[:body]['id'],
          params['id']
        ].join('|')
      end
    end

    post '/users/7?id=9', 'id' => '10'
    assert ok?
    assert_equal '7|9|10|7', body
  end

  it 'exposes original key paths for nested and array params' do
    mock_app do
      get('/') do
        paths = param_sources.paths(:query).sort.join(',')
        "#{paths} => #{param_sources.sources_for('user[name]').join(',')}"
      end
    end

    get '/?user[name]=jo&ids[]=1&ids[]=2'
    assert ok?
    assert_equal 'ids[],user[name] => query', body
  end

  it 'records repeated query keys with their values' do
    mock_app do
      get('/') { param_sources.duplicates.inspect }
    end

    get '/?tag=a&tag=b&ids[]=1&ids[]=2'
    assert ok?
    assert_equal({ 'tag' => %w[a b] }.inspect, body)
  end

  it 'reports cross-source conflicts, including scalar vs array' do
    mock_app do
      get('/users/:id') do
        param_sources.conflicts.map { |c| "#{c.path}:#{c.kind}" }.sort.join(',')
      end
    end

    get '/users/7?id=9'
    assert_equal 'id:value', body

    get '/users/7?id[]=9'
    assert_equal 'id:type', body

    get '/users/7?tag=a&tag=b'
    assert_equal 'tag:duplicate', body
  end

  it 'keeps the default params merge behavior when no policies are declared' do
    mock_app do
      get('/users/:id') { params['id'] }
    end

    get '/users/7?id=9'
    assert ok?
    assert_equal '7', body
  end

  it 'rejects params from disallowed sources before the route block runs' do
    ran = false
    mock_app do
      param_policy 'id', sources: [:route]
      get('/users/:id') do
        ran = true
        'ok'
      end
    end

    get '/users/7?id=9'
    assert_equal 400, status
    refute ran

    get '/users/7'
    assert ok?
    assert ran
  end

  it 'rejects conflicting values across allowed sources' do
    mock_app do
      param_policy 'id', sources: %i[route query], on_conflict: :reject
      get('/users/:id') { 'ok' }
    end

    get '/users/7?id=9'
    assert_equal 400, status

    get '/users/7?id=7'
    assert ok?
  end

  it 'treats scalar vs array across sources as a conflict' do
    mock_app do
      param_policy 'id', sources: %i[route query], on_conflict: :reject
      get('/users/:id') { 'ok' }
    end

    get '/users/7?id[]=9'
    assert_equal 400, status
  end

  it 'rejects disagreeing duplicate query keys' do
    mock_app do
      param_policy 'tag', on_conflict: :reject
      get('/') { 'ok' }
    end

    get '/?tag=a&tag=b'
    assert_equal 400, status

    get '/?tag=a&tag=a'
    assert ok?
  end

  it 'applies the preferred source when the policy names one' do
    mock_app do
      param_policy 'id', sources: %i[route query], on_conflict: :query
      get('/users/:id') { params['id'] }
    end

    get '/users/7?id=9'
    assert ok?
    assert_equal '9', body
  end

  it 'enforces policies on nested parameter paths' do
    mock_app do
      param_policy 'user[name]', sources: [:body]
      post('/users') { params['user']['name'] }
    end

    post '/users?user[name]=jo'
    assert_equal 400, status

    post '/users', 'user' => { 'name' => 'jo' }
    assert ok?
    assert_equal 'jo', body
  end

  it 'does not leak route provenance across passed route candidates' do
    mock_app do
      get('/:one/:two') { pass }
      get('/users/:id') do
        keys = param_sources[:route].keys.sort.join(',')
        "#{keys}:#{param_sources[:route]['id']}:#{params['one'].inspect}"
      end
    end

    get '/users/42'
    assert ok?
    assert_equal 'id:42:nil', body
  end

  it 'raises for unknown sources or conflict strategies' do
    assert_raises(ArgumentError) do
      mock_app { param_policy 'id', sources: [:header] }
    end
    assert_raises(ArgumentError) do
      mock_app { param_policy 'id', on_conflict: :explode }
    end
  end
end
