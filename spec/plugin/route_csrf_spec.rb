require_relative "../spec_helper"

begin
  require 'cgi/escape'
rescue LoadError
  require 'cgi'
end

describe "route_csrf plugin" do 
  include CookieJar

  def route_csrf_app(opts={}, &block)
    app(:bare) do
      send(*DEFAULT_SESSION_ARGS) unless opts[:no_sessions_plugin]
      plugin(:route_csrf, opts, &opts[:block])
      route do |r|
        check_csrf! unless env['SKIP']
        r.post('foo'){'f'}
        r.post('bar'){'b'}
        r.get "token", String do |s|
          csrf_token("/#{s}")
        end
        instance_exec(r, &block) if block
      end
    end
  end

  it "allows all GET requests and allows POST requests only if they have a correct token for the path" do
    route_csrf_app
    token = body("/token/foo")
    token.length.must_equal 84
    body("/foo", "REQUEST_METHOD"=>'POST', 'rack.input'=>rack_input("_csrf=#{Rack::Utils.escape(token)}")).must_equal 'f'
    proc{body("/bar", "REQUEST_METHOD"=>'POST', 'rack.input'=>rack_input("_csrf=#{Rack::Utils.escape(token)}"))}.must_raise Roda::RodaPlugins::RouteCsrf::InvalidToken
    proc{body("/foo", "REQUEST_METHOD"=>'PUT', 'rack.input'=>rack_input("_csrf=#{Rack::Utils.escape(token)}"))}.must_raise Roda::RodaPlugins::RouteCsrf::InvalidToken

    token = body("/token/bar")
    token.length.must_equal 84
    body("/bar", "REQUEST_METHOD"=>'POST', 'rack.input'=>rack_input("_csrf=#{Rack::Utils.escape(token)}")).must_equal 'b'
    proc{body("/foo", "REQUEST_METHOD"=>'POST', 'rack.input'=>rack_input("_csrf=#{Rack::Utils.escape(token)}"))}.must_raise Roda::RodaPlugins::RouteCsrf::InvalidToken
    proc{body("/bar", "REQUEST_METHOD"=>'DELETE', 'rack.input'=>rack_input("_csrf=#{Rack::Utils.escape(token)}"))}.must_raise Roda::RodaPlugins::RouteCsrf::InvalidToken

    # Additional failure cases

    proc{body("/bar", "REQUEST_METHOD"=>'POST', 'rack.input'=>rack_input)}.must_raise Roda::RodaPlugins::RouteCsrf::InvalidToken

    proc{body("/bar", "REQUEST_METHOD"=>'POST', 'rack.input'=>rack_input("_csrf=#{Rack::Utils.escape(token)}a"))}.must_raise Roda::RodaPlugins::RouteCsrf::InvalidToken

    t2 = token.dup
    t2.setbyte(1, t2.getbyte(1) ^ 1)
    proc{body("/bar", "REQUEST_METHOD"=>'POST', 'rack.input'=>rack_input("_csrf=#{Rack::Utils.escape(t2)}"))}.must_raise Roda::RodaPlugins::RouteCsrf::InvalidToken

    t2 = token.dup
    t2.setbyte(61, t2.getbyte(61) ^ 1)
    proc{body("/bar", "REQUEST_METHOD"=>'POST', 'rack.input'=>rack_input("_csrf=#{Rack::Utils.escape(t2)}"))}.must_raise Roda::RodaPlugins::RouteCsrf::InvalidToken

    t2 = token.dup
    t2[1] = '|'
    proc{body("/bar", "REQUEST_METHOD"=>'POST', 'rack.input'=>rack_input("_csrf=#{Rack::Utils.escape(t2)}"))}.must_raise Roda::RodaPlugins::RouteCsrf::InvalidToken
  end

  it "supports overriding csrf_request_method to change how the request method is determined" do
    route_csrf_app{"f"}
    token = body("/token/f")
    body("/f").must_equal "f"
    app.send(:define_method, :csrf_request_method){"POST"}
    proc{body("/f", 'rack.input'=>rack_input(""))}.must_raise Roda::RodaPlugins::RouteCsrf::InvalidToken
    body("/f", "QUERY_STRING"=>"_csrf=#{Rack::Utils.escape(token)}", 'rack.input'=>rack_input("")).must_equal 'f'
  end

  it "supports :check_request_methods option" do
    route_csrf_app(:check_request_methods=>[])
    body("/foo", "REQUEST_METHOD"=>'POST', 'rack.input'=>rack_input("")).must_equal 'f'
    route_csrf_app(:check_request_methods=>["POST"])
    token = body("/token/foo")
    body("/foo", "REQUEST_METHOD"=>'POST', 'rack.input'=>rack_input("_csrf=#{Rack::Utils.escape(token)}")).must_equal 'f'
    proc{body("/bar", "REQUEST_METHOD"=>'POST', 'rack.input'=>rack_input("_csrf=#{Rack::Utils.escape(token)}"))}.must_raise Roda::RodaPlugins::RouteCsrf::InvalidToken
  end

  it "supports :exempt_request_methods option" do
    route_csrf_app(:exempt_request_methods=>["POST"])
    body("/foo", "REQUEST_METHOD"=>'POST', 'rack.input'=>rack_input("")).must_equal 'f'
    route_csrf_app(:exempt_request_methods=>["GET"])
    token = body("/token/foo")
    body("/foo", "REQUEST_METHOD"=>'POST', 'rack.input'=>rack_input("_csrf=#{Rack::Utils.escape(token)}")).must_equal 'f'
    proc{body("/bar", "REQUEST_METHOD"=>'POST', 'rack.input'=>rack_input("_csrf=#{Rack::Utils.escape(token)}"))}.must_raise Roda::RodaPlugins::RouteCsrf::InvalidToken
  end

  it "supports :require_request_specific_tokens => false option to allow non-request-specific tokens" do
    route_csrf_app(:require_request_specific_tokens=>false){csrf_token}
    token = body("/token/foo")
    token.length.must_equal 84
    body("/foo", "REQUEST_METHOD"=>'POST', 'rack.input'=>rack_input("_csrf=#{Rack::Utils.escape(token)}")).must_equal 'f'
    proc{body("/bar", "REQUEST_METHOD"=>'POST', 'rack.input'=>rack_input("_csrf=#{Rack::Utils.escape(token)}"))}.must_raise Roda::RodaPlugins::RouteCsrf::InvalidToken

    token = body
    token.length.must_equal 84
    body("/foo", "REQUEST_METHOD"=>'POST', 'rack.input'=>rack_input("_csrf=#{Rack::Utils.escape(token)}")).must_equal 'f'
    body("/bar", "REQUEST_METHOD"=>'POST', 'rack.input'=>rack_input("_csrf=#{Rack::Utils.escape(token)}")).must_equal 'b'
  end

  it "allows tokens submitted in both parameter and HTTP header if :check_header option is true" do
    route_csrf_app(:check_header=>true)
    token = body("/token/foo")
    token.length.must_equal 84
    body("/foo", "REQUEST_METHOD"=>'POST', 'rack.input'=>rack_input("_csrf=#{Rack::Utils.escape(token)}")).must_equal 'f'
    body("/foo", "REQUEST_METHOD"=>'POST', 'rack.input'=>rack_input, 'HTTP_X_CSRF_TOKEN'=>token).must_equal 'f'
    proc{body("/bar", "REQUEST_METHOD"=>'POST', 'rack.input'=>rack_input("_csrf=#{Rack::Utils.escape(token)}"))}.must_raise Roda::RodaPlugins::RouteCsrf::InvalidToken
    proc{body("/foo", "REQUEST_METHOD"=>'PUT', 'rack.input'=>rack_input, 'HTTP_X_CSRF_TOKEN'=>token)}.must_raise Roda::RodaPlugins::RouteCsrf::InvalidToken
  end

  it "allows tokens submitted in only HTTP header if :check_header option is :only" do
    route_csrf_app(:check_header=>:only)
    token = body("/token/foo")
    token.length.must_equal 84
    body("/foo", "REQUEST_METHOD"=>'POST', 'rack.input'=>rack_input, 'HTTP_X_CSRF_TOKEN'=>token).must_equal 'f'
    proc{body("/foo", "REQUEST_METHOD"=>'POST', 'rack.input'=>rack_input("_csrf=#{Rack::Utils.escape(token)}"))}.must_raise Roda::RodaPlugins::RouteCsrf::InvalidToken
    proc{body("/bar", "REQUEST_METHOD"=>'POST', 'rack.input'=>rack_input("_csrf=#{Rack::Utils.escape(token)}"))}.must_raise Roda::RodaPlugins::RouteCsrf::InvalidToken
    proc{body("/foo", "REQUEST_METHOD"=>'PUT', 'rack.input'=>rack_input, 'HTTP_X_CSRF_TOKEN'=>token)}.must_raise Roda::RodaPlugins::RouteCsrf::InvalidToken
  end

  it "allows tokens specified via :token option to check_csrf" do
    app(:bare) do
      send(*DEFAULT_SESSION_ARGS) unless opts[:no_sessions_plugin]
      plugin(:route_csrf)
      route do |r|
        check_csrf!(:token=>env['TOKEN'])
        r.post('foo'){'f'}
        r.get "token", String do |s|
          csrf_token("/#{s}")
        end
      end
    end
    token = body("/token/foo")
    token.length.must_equal 84
    body("/foo", "REQUEST_METHOD"=>'POST', 'TOKEN'=>token).must_equal 'f'
    proc{body("/foo", "REQUEST_METHOD"=>'POST', 'TOKEN'=>token+'1')}.must_raise Roda::RodaPlugins::RouteCsrf::InvalidToken
  end

  it "allows configuring CSRF failure action with :csrf_failure => :empty_403 option" do
    route_csrf_app(:csrf_failure=>:empty_403)
    body("/foo", "REQUEST_METHOD"=>'POST', 'rack.input'=>rack_input("_csrf=#{Rack::Utils.escape(body("/token/foo"))}")).must_equal 'f'
    req("/foo", "REQUEST_METHOD"=>'POST', 'rack.input'=>rack_input).must_equal [403, {RodaResponseHeaders::CONTENT_TYPE=>'text/html', RodaResponseHeaders::CONTENT_LENGTH=>'0'}, []]
  end

  deprecated "allows configuring CSRF failure action with :csrf_failure => :clear_session option" do
    route_csrf_app(:csrf_failure=>:clear_session){session.inspect}
    body("/foo", "REQUEST_METHOD"=>'POST', 'rack.input'=>rack_input("_csrf=#{Rack::Utils.escape(body("/token/foo"))}")).must_equal 'f'
    body("/b", "REQUEST_METHOD"=>'POST', 'rack.input'=>rack_input("_csrf=#{Rack::Utils.escape(body('/token/a'))}")).must_equal '{}'
  end

  it "allows configuring CSRF failure action with :csrf_failure => proc option" do
    route_csrf_app(:csrf_failure=>proc{|r| r.path + '2'})
    body("/foo", "REQUEST_METHOD"=>'POST', 'rack.input'=>rack_input("_csrf=#{Rack::Utils.escape(body("/token/foo"))}")).must_equal 'f'
    body("/foo", "REQUEST_METHOD"=>'POST', 'rack.input'=>rack_input).must_equal '/foo2'
  end

  it "allows configuring CSRF failure action via a plugin block" do
    route_csrf_app(:block=>proc{|r| r.path + '2'})
    body("/foo", "REQUEST_METHOD"=>'POST', 'rack.input'=>rack_input("_csrf=#{Rack::Utils.escape(body("/token/foo"))}")).must_equal 'f'
    body("/foo", "REQUEST_METHOD"=>'POST', 'rack.input'=>rack_input).must_equal '/foo2'
  end

  it "allows plugin block to integrate with route_block_args plugin" do
    app(:bare) do
      send(*DEFAULT_SESSION_ARGS)
      plugin :route_block_args do
        [request, request.path, response]
      end
      plugin(:route_csrf){|r, path, res| res.write(path); res.write('2')}
      route do |r, path, res|
        check_csrf!
        r.post('foo'){'f'}
        r.get "token", String do |s|
          csrf_token("/#{s}")
        end
      end
    end
    body("/foo", "REQUEST_METHOD"=>'POST', 'rack.input'=>rack_input("_csrf=#{Rack::Utils.escape(body("/token/foo"))}")).must_equal 'f'
    body("/foo", "REQUEST_METHOD"=>'POST', 'rack.input'=>rack_input).must_equal '/foo2'
  end

  it "raises Error if configuring plugin with invalid :csrf_failure option" do
    route_csrf_app(:csrf_failure=>:foo)
    proc{body("/foo", "REQUEST_METHOD"=>'POST', 'rack.input'=>rack_input)}.must_raise Roda::RodaError
  end

  it "raises Error if configuring plugin with block and :csrf_failure option" do
    proc{route_csrf_app(:block=>proc{|r| r.path + '2'}, :csrf_failure=>:raise)}.must_raise Roda::RodaError
  end

  deprecated "supports check_csrf! :csrf_failure option as a Proc" do
    pr = proc{env['BAD'] == '1' ? 't' : 'f'}
    route_csrf_app{check_csrf!(:csrf_failure=>pr); ''}
    token = body("/token/foo")
    body("SKIP"=>"1", "BAD"=>'1', "REQUEST_METHOD"=>'POST', 'rack.input'=>rack_input("_csrf=#{Rack::Utils.escape(token)}")).must_equal 't'
    body("SKIP"=>"1", "BAD"=>'0', "REQUEST_METHOD"=>'POST', 'rack.input'=>rack_input("_csrf=#{Rack::Utils.escape(token)}")).must_equal 'f'
  end

  it "supports valid_csrf? method" do
    route_csrf_app{valid_csrf?.to_s}
    body("/a", "REQUEST_METHOD"=>'POST', 'rack.input'=>rack_input("_csrf=#{Rack::Utils.escape(body("/token/a"))}")).must_equal 'true'
    body("/a", "REQUEST_METHOD"=>'POST', 'SKIP'=>'1', 'rack.input'=>rack_input("_csrf=#{Rack::Utils.escape(body("/token/b"))}")).must_equal 'false'
  end

  it "supports valid_csrf? method" do
    route_csrf_app do
      check_csrf!{'nope'}
      'yep'
    end
    body("/a", "REQUEST_METHOD"=>'POST', 'SKIP'=>'1', 'rack.input'=>rack_input("_csrf=#{Rack::Utils.escape(body("/token/a"))}")).must_equal 'yep'
    body("/a", "REQUEST_METHOD"=>'POST', 'SKIP'=>'1', 'rack.input'=>rack_input("_csrf=#{Rack::Utils.escape(body("/token/b"))}")).must_equal 'nope'
  end

  it "supports use_request_specific_csrf_tokens? method" do
    route_csrf_app{use_request_specific_csrf_tokens?.to_s}
    body.must_equal 'true'
    route_csrf_app(:require_request_specific_tokens=>false){use_request_specific_csrf_tokens?.to_s}
    body.must_equal 'false'
  end

  it "supports csrf_field method" do
    route_csrf_app{csrf_field}
    body.must_equal '_csrf'
    route_csrf_app(:field=>'foo'){csrf_field}
    body.must_equal 'foo'
  end

  it "supports csrf_header method" do
    route_csrf_app{csrf_header}
    body.must_equal 'X-CSRF-Token'
    route_csrf_app(:header=>'Foo'){csrf_header}
    body.must_equal 'Foo'
  end

  it "supports csrf_metatag method" do
    route_csrf_app(:require_request_specific_tokens=>false){csrf_metatag}
    body =~ /\A<meta name="_csrf" content="([+\/0-9A-Za-z]{84})" \/>\z/
    body("/foo", "REQUEST_METHOD"=>'POST', 'rack.input'=>rack_input("_csrf=#{Rack::Utils.escape($1)}")).must_equal 'f'

    route_csrf_app(:require_request_specific_tokens=>false, :field=>'foo'){csrf_metatag}
    body =~ /\A<meta name="foo" content="([+\/0-9A-Za-z]{84})" \/>\z/
    body("/bar", "REQUEST_METHOD"=>'POST', 'rack.input'=>rack_input("foo=#{Rack::Utils.escape($1)}")).must_equal 'b'
  end

  it "supports csrf_tag method" do
    route_csrf_app(:require_request_specific_tokens=>false){csrf_tag}
    body =~ /\A<input type="hidden" name="_csrf" value="([+\/0-9A-Za-z]{84})" \/>\z/
    body("/foo", "REQUEST_METHOD"=>'POST', 'rack.input'=>rack_input("_csrf=#{Rack::Utils.escape($1)}")).must_equal 'f'

    route_csrf_app(:require_request_specific_tokens=>false, :field=>'foo'){csrf_tag}
    body =~ /\A<input type="hidden" name="foo" value="([+\/0-9A-Za-z]{84})" \/>\z/
    body("/foo", "REQUEST_METHOD"=>'POST', 'rack.input'=>rack_input("foo=#{Rack::Utils.escape($1)}")).must_equal 'f'

    route_csrf_app{csrf_tag('/foo')}
    body =~ /\A<input type="hidden" name="_csrf" value="([+\/0-9A-Za-z]{84})" \/>\z/
    token = $1
    body("/foo", "REQUEST_METHOD"=>'POST', 'rack.input'=>rack_input("_csrf=#{Rack::Utils.escape(token)}")).must_equal 'f'
    proc{body("/bar", "REQUEST_METHOD"=>'POST', 'rack.input'=>rack_input("_csrf=#{Rack::Utils.escape(token)}"))}.must_raise Roda::RodaPlugins::RouteCsrf::InvalidToken
    proc{body("/foo", "REQUEST_METHOD"=>'PUT', 'rack.input'=>rack_input, 'QUERY_STRING'=>"_csrf=#{Rack::Utils.escape(token)}")}.must_raise Roda::RodaPlugins::RouteCsrf::InvalidToken

    route_csrf_app do |r|
      r.is 'foo', :method=>'PUT' do
        'f2'
      end
      csrf_tag('/foo', 'PUT')
    end
    body =~ /\A<input type="hidden" name="_csrf" value="([+\/0-9A-Za-z]{84})" \/>\z/
    token = $1
    body("/foo", "REQUEST_METHOD"=>'PUT', 'rack.input'=>rack_input, 'QUERY_STRING'=>"_csrf=#{Rack::Utils.escape(token)}").must_equal 'f2'
    proc{body("/bar", "REQUEST_METHOD"=>'PUT', 'rack.input'=>rack_input, 'QUERY_STRING'=>"_csrf=#{Rack::Utils.escape(token)}")}.must_raise Roda::RodaPlugins::RouteCsrf::InvalidToken
    proc{body("/foo", "REQUEST_METHOD"=>'POST', 'rack.input'=>rack_input("_csrf=#{Rack::Utils.escape(token)}"))}.must_raise Roda::RodaPlugins::RouteCsrf::InvalidToken
  end

  it "supports csrf_formaction_tag method" do
    route_csrf_app{csrf_formaction_tag('/foo')}
    body =~ /\A<input type="hidden" name="([^"]+)" value="([+\/0-9A-Za-z]{84})" \/>\z/
    field = CGI.unescapeHTML($1)
    token = $2
    body("/foo", "REQUEST_METHOD"=>'POST', 'rack.input'=>rack_input("#{Rack::Utils.escape(field)}=#{Rack::Utils.escape(token)}")).must_equal 'f'
    proc{body("/bar", "REQUEST_METHOD"=>'POST', 'rack.input'=>rack_input("#{Rack::Utils.escape(field)}=#{Rack::Utils.escape(token)}"))}.must_raise Roda::RodaPlugins::RouteCsrf::InvalidToken
    proc{body("/foo", "REQUEST_METHOD"=>'PUT', 'rack.input'=>rack_input, 'QUERY_STRING'=>"#{Rack::Utils.escape(field)}=#{Rack::Utils.escape(token)}")}.must_raise Roda::RodaPlugins::RouteCsrf::InvalidToken

    route_csrf_app do |r|
      r.is 'foo', :method=>'PUT' do
        'f2'
      end
      csrf_formaction_tag('/foo', 'PUT')
    end
    body =~ /\A<input type="hidden" name="([^"]+)" value="([+\/0-9A-Za-z]{84})" \/>\z/
    field = CGI.unescapeHTML($1)
    token = $2
    body("/foo", "REQUEST_METHOD"=>'PUT', 'rack.input'=>rack_input, 'QUERY_STRING'=>"#{Rack::Utils.escape(field)}=#{Rack::Utils.escape(token)}").must_equal 'f2'
    proc{body("/bar", "REQUEST_METHOD"=>'PUT', 'rack.input'=>rack_input, 'QUERY_STRING'=>"#{Rack::Utils.escape(field)}=#{Rack::Utils.escape(token)}")}.must_raise Roda::RodaPlugins::RouteCsrf::InvalidToken
    proc{body("/foo", "REQUEST_METHOD"=>'POST', 'rack.input'=>rack_input("#{Rack::Utils.escape(field)}=#{Rack::Utils.escape(token)}"))}.must_raise Roda::RodaPlugins::RouteCsrf::InvalidToken
  end

  it "supports csrf_token method" do
    route_csrf_app(:require_request_specific_tokens=>false){csrf_token}
    body("/foo", "REQUEST_METHOD"=>'POST', 'rack.input'=>rack_input("_csrf=#{Rack::Utils.escape(body)}")).must_equal 'f'

    route_csrf_app(:require_request_specific_tokens=>false, :field=>'foo'){csrf_token}
    body("/foo", "REQUEST_METHOD"=>'POST', 'rack.input'=>rack_input("foo=#{Rack::Utils.escape(body)}")).must_equal 'f'

    route_csrf_app{csrf_token('/foo')}
    token = body
    body("/foo", "REQUEST_METHOD"=>'POST', 'rack.input'=>rack_input("_csrf=#{Rack::Utils.escape(token)}")).must_equal 'f'
    proc{body("/bar", "REQUEST_METHOD"=>'POST', 'rack.input'=>rack_input("_csrf=#{Rack::Utils.escape(token)}"))}.must_raise Roda::RodaPlugins::RouteCsrf::InvalidToken
    proc{body("/foo", "REQUEST_METHOD"=>'PUT', 'rack.input'=>rack_input, 'QUERY_STRING'=>"_csrf=#{Rack::Utils.escape(token)}")}.must_raise Roda::RodaPlugins::RouteCsrf::InvalidToken

    route_csrf_app do |r|
      r.is 'foo', :method=>'PUT' do
        'f2'
      end
      csrf_token('/foo', 'PUT')
    end
    token = body
    body("/foo", "REQUEST_METHOD"=>'PUT', 'rack.input'=>rack_input, 'QUERY_STRING'=>"_csrf=#{Rack::Utils.escape(token)}").must_equal 'f2'
    proc{body("/bar", "REQUEST_METHOD"=>'PUT', 'rack.input'=>rack_input, 'QUERY_STRING'=>"_csrf=#{Rack::Utils.escape(token)}")}.must_raise Roda::RodaPlugins::RouteCsrf::InvalidToken
    proc{body("/foo", "REQUEST_METHOD"=>'POST', 'rack.input'=>rack_input("_csrf=#{Rack::Utils.escape(token)}"))}.must_raise Roda::RodaPlugins::RouteCsrf::InvalidToken
  end

  it "supports csrf_path method" do
    route_csrf_app do |r|
      r.post{r.path + '2'}
      csrf_token(csrf_path(env['CP']))
    end

    body("REQUEST_METHOD"=>'POST', 'rack.input'=>rack_input("_csrf=#{Rack::Utils.escape(body)}")).must_equal '/2'
    body("REQUEST_METHOD"=>'POST', 'rack.input'=>rack_input("_csrf=#{Rack::Utils.escape(body('CP'=>''))}")).must_equal '/2'
    body("REQUEST_METHOD"=>'POST', 'rack.input'=>rack_input("_csrf=#{Rack::Utils.escape(body('CP'=>'#foo'))}")).must_equal '/2'
    body("REQUEST_METHOD"=>'POST', 'rack.input'=>rack_input("_csrf=#{Rack::Utils.escape(body('CP'=>'?foo'))}")).must_equal '/2'

    body('/a', "REQUEST_METHOD"=>'POST', 'rack.input'=>rack_input("_csrf=#{Rack::Utils.escape(body('/a'))}")).must_equal '/a2'
    body('/a', "REQUEST_METHOD"=>'POST', 'rack.input'=>rack_input("_csrf=#{Rack::Utils.escape(body('/a', 'CP'=>''))}")).must_equal '/a2'
    body('/a', "REQUEST_METHOD"=>'POST', 'rack.input'=>rack_input("_csrf=#{Rack::Utils.escape(body('/a', 'CP'=>'?foo'))}")).must_equal '/a2'
    body('/a', "REQUEST_METHOD"=>'POST', 'rack.input'=>rack_input("_csrf=#{Rack::Utils.escape(body('/a', 'CP'=>'#foo'))}")).must_equal '/a2'

    body("REQUEST_METHOD"=>'POST', 'rack.input'=>rack_input("_csrf=#{Rack::Utils.escape(body('CP'=>'http://foo/'))}")).must_equal '/2'
    body('/a', "REQUEST_METHOD"=>'POST', 'rack.input'=>rack_input("_csrf=#{Rack::Utils.escape(body('CP'=>'https://foo/a'))}")).must_equal '/a2'
    body('/a/b', "REQUEST_METHOD"=>'POST', 'rack.input'=>rack_input("_csrf=#{Rack::Utils.escape(body('CP'=>'http://foo/a/b'))}")).must_equal '/a/b2'

    body("REQUEST_METHOD"=>'POST', 'rack.input'=>rack_input("_csrf=#{Rack::Utils.escape(body('CP'=>'/'))}")).must_equal '/2'
    body('/a', "REQUEST_METHOD"=>'POST', 'rack.input'=>rack_input("_csrf=#{Rack::Utils.escape(body('CP'=>'/a'))}")).must_equal '/a2'
    body('/a/b', "REQUEST_METHOD"=>'POST', 'rack.input'=>rack_input("_csrf=#{Rack::Utils.escape(body('CP'=>'/a/b'))}")).must_equal '/a/b2'

    body('/a', "REQUEST_METHOD"=>'POST', 'rack.input'=>rack_input("_csrf=#{Rack::Utils.escape(body('HTTPS'=>'on', 'HTTP_HOST'=>'foo.com', 'CP'=>'a'))}")).must_equal '/a2'
    body('/a', "REQUEST_METHOD"=>'POST', 'rack.input'=>rack_input("_csrf=#{Rack::Utils.escape(body('/b', 'HTTPS'=>'on', 'HTTP_HOST'=>'foo.com', 'CP'=>'a'))}")).must_equal '/a2'
    body('/b/a', "REQUEST_METHOD"=>'POST', 'rack.input'=>rack_input("_csrf=#{Rack::Utils.escape(body('/b/', 'HTTPS'=>'on', 'HTTP_HOST'=>'foo.com', 'CP'=>'a'))}")).must_equal '/b/a2'
    body('/b/a', "REQUEST_METHOD"=>'POST', 'rack.input'=>rack_input("_csrf=#{Rack::Utils.escape(body('/b/b', 'HTTPS'=>'on', 'HTTP_HOST'=>'foo.com', 'CP'=>'a'))}")).must_equal '/b/a2'
    body('/a/b', "REQUEST_METHOD"=>'POST', 'rack.input'=>rack_input("_csrf=#{Rack::Utils.escape(body('/b', 'HTTPS'=>'on', 'HTTP_HOST'=>'foo.com', 'CP'=>'a/b'))}")).must_equal '/a/b2'
    body('/b/a/b', "REQUEST_METHOD"=>'POST', 'rack.input'=>rack_input("_csrf=#{Rack::Utils.escape(body('/b/', 'HTTPS'=>'on', 'HTTP_HOST'=>'foo.com', 'CP'=>'a/b'))}")).must_equal '/b/a/b2'
    body('/b/a/b', "REQUEST_METHOD"=>'POST', 'rack.input'=>rack_input("_csrf=#{Rack::Utils.escape(body('/b/a', 'HTTPS'=>'on', 'HTTP_HOST'=>'foo.com', 'CP'=>'a/b'))}")).must_equal '/b/a/b2'
  end

  it "supports csrf_token :fields option for binding tokens to the submitted form field structure" do
    app(:bare) do
      send(*DEFAULT_SESSION_ARGS)
      plugin(:route_csrf)
      route do |r|
        check_csrf!
        r.post('update'){'u'}
        r.post('other'){'o'}
        r.get "token" do
          csrf_token('/update', 'POST', :fields=>r.params['fields'].split(','))
        end
        r.get "tag" do
          csrf_tag('/update', 'POST', :fields=>%w'name email')
        end
        r.get "plain_token" do
          csrf_token('/update')
        end
      end
    end

    esc = lambda{|t| Rack::Utils.escape(t)}
    submit = lambda do |input, env={}|
      body("/update", {"REQUEST_METHOD"=>'POST', 'rack.input'=>rack_input(input)}.merge(env))
    end

    # Token for the restricted form (only name and email may be submitted)
    token = body("/token", "QUERY_STRING"=>"fields=name,email")
    token.length.must_equal 86

    # Matching structure is accepted, and field values are not signed
    submit.call("_csrf=#{esc.call(token)}&name=x&email=y").must_equal 'u'
    submit.call("_csrf=#{esc.call(token)}&name=admin&email=evil%40example.com").must_equal 'u'

    # Field order does not matter
    submit.call("_csrf=#{esc.call(token)}&email=y&name=x").must_equal 'u'

    # Duplicate fields do not change the structure
    submit.call("_csrf=#{esc.call(token)}&name=x&name=z&email=y").must_equal 'u'

    # Query string parameters are not part of the structure
    submit.call("_csrf=#{esc.call(token)}&name=x&email=y", "QUERY_STRING"=>'admin=1').must_equal 'u'

    # Adding a field (e.g. an injected hidden field) is rejected
    proc{submit.call("_csrf=#{esc.call(token)}&name=x&email=y&admin=1")}.must_raise Roda::RodaPlugins::RouteCsrf::InvalidToken

    # Removing or renaming fields is rejected
    proc{submit.call("_csrf=#{esc.call(token)}&name=x")}.must_raise Roda::RodaPlugins::RouteCsrf::InvalidToken
    proc{submit.call("_csrf=#{esc.call(token)}&name=x&email2=y")}.must_raise Roda::RodaPlugins::RouteCsrf::InvalidToken

    # Token for the more privileged form on the same path
    admin_token = body("/token", "QUERY_STRING"=>"fields=name,email,admin")
    submit.call("_csrf=#{esc.call(admin_token)}&name=x&email=y&admin=1").must_equal 'u'

    # The privileged token cannot be reused for the restricted structure,
    # and the restricted token cannot be reused for the privileged structure
    proc{submit.call("_csrf=#{esc.call(admin_token)}&name=x&email=y")}.must_raise Roda::RodaPlugins::RouteCsrf::InvalidToken
    proc{submit.call("_csrf=#{esc.call(token)}&name=x&email=y&admin=1")}.must_raise Roda::RodaPlugins::RouteCsrf::InvalidToken

    # Bound tokens are still specific to the request method and path
    proc{body("/other", "REQUEST_METHOD"=>'POST', 'rack.input'=>rack_input("_csrf=#{esc.call(token)}&name=x&email=y"))}.must_raise Roda::RodaPlugins::RouteCsrf::InvalidToken
    proc{body("/update", "REQUEST_METHOD"=>'PUT', 'rack.input'=>rack_input("_csrf=#{esc.call(token)}&name=x&email=y"))}.must_raise Roda::RodaPlugins::RouteCsrf::InvalidToken

    # Tampered bound tokens are rejected
    t2 = token.dup
    t2.setbyte(1, t2.getbyte(1) ^ 1)
    proc{submit.call("_csrf=#{esc.call(t2)}&name=x&email=y")}.must_raise Roda::RodaPlugins::RouteCsrf::InvalidToken
    t2 = token.dup
    t2.setbyte(62, t2.getbyte(62) ^ 1)
    proc{submit.call("_csrf=#{esc.call(t2)}&name=x&email=y")}.must_raise Roda::RodaPlugins::RouteCsrf::InvalidToken

    # csrf_tag supports the :fields option
    body("/tag") =~ /\A<input type="hidden" name="_csrf" value="([+\/0-9A-Za-z]{86})" \/>\z/
    tag_token = $1
    submit.call("_csrf=#{esc.call(tag_token)}&name=x&email=y").must_equal 'u'
    proc{submit.call("_csrf=#{esc.call(tag_token)}&name=x&email=y&admin=1")}.must_raise Roda::RodaPlugins::RouteCsrf::InvalidToken

    # Unbound tokens keep the existing protocol and still work
    plain_token = body("/plain_token")
    plain_token.length.must_equal 84
    submit.call("_csrf=#{esc.call(plain_token)}&name=x&email=y&admin=1").must_equal 'u'
  end

  it "supports nested field paths and arrays in csrf_token :fields option" do
    app(:bare) do
      send(*DEFAULT_SESSION_ARGS)
      plugin(:route_csrf)
      route do |r|
        check_csrf!
        r.post('update'){'u'}
        r.get "token" do
          csrf_token('/update', 'POST', :fields=>r.params['fields'].split(','))
        end
      end
    end

    esc = lambda{|t| Rack::Utils.escape(t)}
    submit = lambda{|input| body("/update", "REQUEST_METHOD"=>'POST', 'rack.input'=>rack_input(input))}
    fields = lambda{|f| body("/token", "QUERY_STRING"=>"fields=#{esc.call(f)}")}

    token = fields.call('user[name],user[tags][]')
    submit.call("_csrf=#{esc.call(token)}&user[name]=x&user[tags][]=a&user[tags][]=b").must_equal 'u'

    # Array length is not significant
    submit.call("_csrf=#{esc.call(token)}&user[name]=x&user[tags][]=a").must_equal 'u'

    # Arrays of hashes merge the declared element structures
    items_token = fields.call('items[][name],items[][price]')
    submit.call("_csrf=#{esc.call(items_token)}&items[][name]=a&items[][price]=1&items[][name]=b&items[][price]=2").must_equal 'u'

    # Missing nested fields are rejected
    proc{submit.call("_csrf=#{esc.call(token)}&user[name]=x")}.must_raise Roda::RodaPlugins::RouteCsrf::InvalidToken

    # Deeper nesting than declared is rejected
    proc{submit.call("_csrf=#{esc.call(token)}&user[name][first]=x&user[tags][]=a")}.must_raise Roda::RodaPlugins::RouteCsrf::InvalidToken

    # A submitted hash does not match an array declaration
    proc{submit.call("_csrf=#{esc.call(token)}&user[name]=x&user[tags][0]=a")}.must_raise Roda::RodaPlugins::RouteCsrf::InvalidToken

    # A submitted scalar does not match a hash declaration
    proc{submit.call("_csrf=#{esc.call(token)}&user=x")}.must_raise Roda::RodaPlugins::RouteCsrf::InvalidToken

    # Canonicalization is unambiguous: keys are length-prefixed, so
    # repartitioning field names cannot produce a matching structure
    split_token = fields.call('a,bc')
    submit.call("_csrf=#{esc.call(split_token)}&a=1&bc=2").must_equal 'u'
    proc{submit.call("_csrf=#{esc.call(split_token)}&ab=1&c=2")}.must_raise Roda::RodaPlugins::RouteCsrf::InvalidToken
  end

  it "raises RodaError for invalid csrf_token :fields option usage" do
    app(:bare) do
      send(*DEFAULT_SESSION_ARGS)
      plugin(:route_csrf)
      route do |r|
        case r.path
        when '/no_path'
          csrf_token(nil, nil, :fields=>['a'])
        when '/conflict'
          csrf_token('/update', 'POST', :fields=>['a', 'a[b]'])
        when '/conflict_array'
          csrf_token('/update', 'POST', :fields=>['a[]', 'a[b]'])
        when '/invalid'
          csrf_token('/update', 'POST', :fields=>['a][b'])
        end
      end
    end
    proc{body('/no_path')}.must_raise Roda::RodaError
    proc{body('/conflict')}.must_raise Roda::RodaError
    proc{body('/conflict_array')}.must_raise Roda::RodaError
    proc{body('/invalid')}.must_raise Roda::RodaError
  end

begin
  require 'rack/csrf'
rescue LoadError
  warn "rack_csrf not installed, skipping route_csrf plugin test for rack_csrf upgrade"  
else
  it "supports upgrades from existing rack_csrf token" do
    route_csrf_app(:upgrade_from_rack_csrf_key=>'csrf.token', :no_sessions_plugin=>true) do |r|
      r.get 'clear' do
        session.clear
        ''
      end
      Rack::Csrf.token(env)
    end
    app.use(*DEFAULT_SESSION_MIDDLEWARE_ARGS)
    app.use Rack::Csrf, :skip=>['POST:/foo', 'POST:/bar'], :raise=>true
    token = body
    token.length.wont_equal 84
    body("/foo", "REQUEST_METHOD"=>'POST', 'rack.input'=>rack_input("_csrf=#{Rack::Utils.escape(token)}")).must_equal 'f'
    body("/bar", "REQUEST_METHOD"=>'POST', 'rack.input'=>rack_input("_csrf=#{Rack::Utils.escape(token)}")).must_equal 'b'
    body('/clear').must_equal ''
    proc{body("/foo", "REQUEST_METHOD"=>'POST', 'rack.input'=>rack_input("_csrf=#{Rack::Utils.escape(token)}"))}.must_raise Roda::RodaPlugins::RouteCsrf::InvalidToken
    proc{body("/bar", "REQUEST_METHOD"=>'POST', 'rack.input'=>rack_input("_csrf=#{Rack::Utils.escape(token)}"))}.must_raise Roda::RodaPlugins::RouteCsrf::InvalidToken
  end
end
end
