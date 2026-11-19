require_relative "../spec_helper"

describe "default_headers plugin" do 
  h = {RodaResponseHeaders::CONTENT_TYPE=>'text/json', 'foo'=>'bar'}.freeze
  rh_html = {RodaResponseHeaders::CONTENT_TYPE=>'text/html', 'foo'=>'bar'}.freeze

  it "sets the default headers to use for the response" do
    app(:bare) do
      plugin :default_headers, h
      route do |r|
        r.halt response.finish_with_body([])
      end
    end

    req[1].must_equal h
    req[1].wont_be_same_as h 
  end

  it "should not override existing default headers" do
    app(:bare) do
      plugin :default_headers, h
      plugin :default_headers

      route do |r|
        r.halt response.finish_with_body([])
      end
    end

    req[1].must_equal h
  end

  it "should handle case insensitive headers" do
    app(:bare) do
      plugin :default_headers, 'Content-Type' => 'application/json'

      route do |r|
        ''
      end
    end

    header(RodaResponseHeaders::CONTENT_TYPE).must_equal "application/json"
  end

  it "should allow modifying the default headers by reloading the plugin" do
    app(:bare) do
      plugin :default_headers, RodaResponseHeaders::CONTENT_TYPE => 'text/json'
      plugin :default_headers, 'foo' => 'bar'

      route do |r|
        r.halt response.finish_with_body([])
      end
    end

    req[1].must_equal h
  end

  it "should have a default Content-Type header" do
    app(:bare) do
      plugin :default_headers, 'foo'=>'bar'

      route do |r|
        r.halt response.finish_with_body([])
      end
    end

    req[1].must_equal rh_html
  end

  it "should work correctly in subclasses" do
    app(:bare) do
      plugin :default_headers, h

      route do |r|
        r.halt response.finish_with_body([])
      end
    end

    @app = Class.new(@app)

    req[1].must_equal h
  end

  it "should offer default_headers method on class and response instance" do
    app.plugin :default_headers, h
    app.default_headers.must_equal h
    app::RodaResponse.new.default_headers.must_equal h
  end

  it "should work correctly when frozen" do
    app(:bare) do
      plugin :default_headers, h
      route do |r|
        r.halt response.finish_with_body([])
      end
    end

    req[1].must_equal h
    req[1].wont_be_same_as h 

    app.freeze
    req[1].must_equal h
    req[1].wont_be_same_as h 
  end

  it "supports setting non-strings as header values" do
    headers = {RodaResponseHeaders::CONTENT_TYPE=>'text/json', 'foo'=>:bar}

    app(:bare) do
      plugin :default_headers, h
      route do |r|
        r.halt response.finish_with_body([])
      end
    end

    req[1].must_equal h
    req[1].wont_be_same_as headers
  end unless ENV['LINT']

  it "should work when freezing" do
    app(:bare) do
      plugin :default_headers, 'foo'=>'bar'

      route do |r|
        r.halt response.finish_with_body([])
      end
    end

    req[1].must_equal rh_html
    app.freeze
    req[1].must_equal rh_html
  end

  it "should work when freezing when not all headers are strings" do
    app(:bare) do
      plugin :default_headers, 'foo'=>:bar

      route do |r|
        r.halt response.finish_with_body([])
      end
    end

    req[1].must_equal(RodaResponseHeaders::CONTENT_TYPE=>'text/html', 'foo'=>:bar)
    app.freeze
    req[1].must_equal(RodaResponseHeaders::CONTENT_TYPE=>'text/html', 'foo'=>:bar)
  end unless ENV['LINT']

  it "should work when subclassing and redefining" do
    app(:bare) do
      plugin :default_headers, 'foo'=>'bar'

      route do |r|
        r.halt response.finish_with_body([])
      end
    end

    req[1].must_equal rh_html

    app = self.app
    app2 = Class.new(app)
    app2.plugin(:default_headers, 'foo'=>'bar2')

    req[1].must_equal rh_html
    
    @app = app2
    req[1].must_equal(RodaResponseHeaders::CONTENT_TYPE=>'text/html', 'foo'=>'bar2')

    app.plugin(:default_headers, 'foo'=>'bar3')
    req[1].must_equal(RodaResponseHeaders::CONTENT_TYPE=>'text/html', 'foo'=>'bar2')

    @app = app
    req[1].must_equal(RodaResponseHeaders::CONTENT_TYPE=>'text/html', 'foo'=>'bar3')
  end

  [true, false].each do |freeze|
    [true, false].each do |after|
      it "should work with content_security_policy plugin if loaded #{after ? 'after' : 'before'}#{' when freezing' if freeze}" do
        headers = {'foo'=>'bar'}

        app(:bare) do
          plugin :default_headers, headers if after 
          plugin :content_security_policy do |csp|
            csp.default_src :none
          end
          plugin :default_headers, headers unless after 

          route do |r|
            r.halt response.finish_with_body([])
          end
        end

        req[1].must_equal(RodaResponseHeaders::CONTENT_TYPE=>'text/html', 'foo'=>'bar', RodaResponseHeaders::CONTENT_SECURITY_POLICY=>"default-src 'none'; ")

        app = self.app
        app2 = Class.new(app)
        app2.plugin(:default_headers, 'foo'=>'bar2')

        req[1].must_equal(RodaResponseHeaders::CONTENT_TYPE=>'text/html', 'foo'=>'bar', RodaResponseHeaders::CONTENT_SECURITY_POLICY=>"default-src 'none'; ")
        @app.freeze if freeze
        req[1].must_equal(RodaResponseHeaders::CONTENT_TYPE=>'text/html', 'foo'=>'bar', RodaResponseHeaders::CONTENT_SECURITY_POLICY=>"default-src 'none'; ")
        
        @app = app2
        req[1].must_equal(RodaResponseHeaders::CONTENT_TYPE=>'text/html', 'foo'=>'bar2', RodaResponseHeaders::CONTENT_SECURITY_POLICY=>"default-src 'none'; ")
        @app.freeze if freeze
        req[1].must_equal(RodaResponseHeaders::CONTENT_TYPE=>'text/html', 'foo'=>'bar2', RodaResponseHeaders::CONTENT_SECURITY_POLICY=>"default-src 'none'; ")
      end
    end
  end
end
