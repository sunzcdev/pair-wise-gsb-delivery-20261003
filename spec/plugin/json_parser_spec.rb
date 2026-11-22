require_relative "../spec_helper"

describe "json_parser plugin" do 
  deprecated "parses incoming json if content type specifies json" do
    app(:json_parser){|r| r.params['a']['b'].to_s}
    body('rack.input'=>rack_input('{"a":{"b":1}}'), 'CONTENT_TYPE'=>'text/json', 'REQUEST_METHOD'=>'POST').must_equal '1'
  end
end

describe "json_parser plugin" do 
  def json_parser_app(&block)
    app(:bare) do
      plugin :json_parser, :content_type_regexp=>/\Aapplication\/json\b/i
      route(&block)
    end
  end

  before do
    json_parser_app{|r| r.params['a']['b'].to_s}
  end

  it "parses incoming json if content type specifies json" do
    body('rack.input'=>rack_input('{"a":{"b":1}}'), 'CONTENT_TYPE'=>'application/json', 'REQUEST_METHOD'=>'POST').must_equal '1'
  end

  it "Handles Rack::Request#POST being called in advance" do
    env = req_env('rack.input'=>rack_input('{"a":{"b":1}}'), 'CONTENT_TYPE'=>'application/json', 'REQUEST_METHOD'=>'POST')
    r = Rack::Request.new(env)
    r.POST
    body(env).must_equal '1'
  end

  it "doesn't affect parsing of non-json content type" do
    body('rack.input'=>rack_input('a[b]=1'), 'REQUEST_METHOD'=>'POST').must_equal '1'
  end

  it "parses incoming json if content type specifies json and body is already read" do
    @app.route do |r|
      r.body.read
      r.params['a']['b'].to_s
    end
    body('rack.input'=>rack_input('{"a":{"b":1}}'), 'CONTENT_TYPE'=>'application/json', 'REQUEST_METHOD'=>'POST').must_equal '1'
  end unless Rack.release >= '2.3'

  it "returns 400 for invalid json" do
    req('rack.input'=>rack_input('{"a":{"b":1}'), 'CONTENT_TYPE'=>'application/json', 'REQUEST_METHOD'=>'POST').must_equal [400, {}, []]
  end

  it "returns 400 for invalid json when using params_capturing plugin" do
    @app.plugin :params_capturing
    req('rack.input'=>rack_input('{"a":{"b":1}'), 'CONTENT_TYPE'=>'application/json', 'REQUEST_METHOD'=>'POST').must_equal [400, {}, []]
  end

  it "raises by default if r.params is called and a non-hash is submitted" do
    proc do
      req('rack.input'=>rack_input('[1]'), 'CONTENT_TYPE'=>'application/json', 'REQUEST_METHOD'=>'POST')
    end.must_raise
  end
end

describe "json_parser plugin" do 
  def json_parser_app(&block)
    app(:bare) do
      plugin :json_parser, :content_type_regexp=>/\Aapplication\/json\b/i
      route(&block)
    end
  end

  it "handles empty request bodies" do
    json_parser_app do |r|
      r.params.length.to_s
    end
    body('rack.input'=>rack_input(''), 'CONTENT_TYPE'=>'application/json', 'REQUEST_METHOD'=>'POST').must_equal '0'
  end

  it "handles arrays and other non-hash values using r.POST" do
    json_parser_app do |r|
      r.POST.inspect
    end
    body('rack.input'=>rack_input('[ 1 ]'), 'CONTENT_TYPE'=>'application/json', 'REQUEST_METHOD'=>'POST').must_equal '[1]'
  end

  it "supports :wrap=>:always option" do
    app(:bare) do
      plugin(:json_parser, :wrap=>:always, :content_type_regexp=>/\Aapplication\/json\b/i)
      route do |r|
        r.post 'a' do r.params['_json']['a']['b'].to_s end
        r.params['_json'][1].to_s
      end
    end
    body('/a', 'rack.input'=>rack_input('{"a":{"b":1}}'), 'CONTENT_TYPE'=>'application/json', 'REQUEST_METHOD'=>'POST').must_equal '1'
    body('rack.input'=>rack_input('[true, 2]'), 'CONTENT_TYPE'=>'application/json', 'REQUEST_METHOD'=>'POST').must_equal '2'
  end

  it "supports :wrap=>:unless_hash option" do
    app(:bare) do
      plugin(:json_parser, :wrap=>:unless_hash, :content_type_regexp=>/\Aapplication\/json\b/i)
      route do |r|
        r.post 'a' do r.params['a']['b'].to_s end
        r.params['_json'][1].to_s
      end
    end
    body('/a', 'rack.input'=>rack_input('{"a":{"b":1}}'), 'CONTENT_TYPE'=>'application/json', 'REQUEST_METHOD'=>'POST').must_equal '1'
    body('rack.input'=>rack_input('[true, 2]'), 'CONTENT_TYPE'=>'application/json', 'REQUEST_METHOD'=>'POST').must_equal '2'
  end

  it "raises for unsupported :wrap option" do
    proc do 
      app(:bare) do
        plugin(:json_parser, :wrap=>:foo, :content_type_regexp=>/\Aapplication\/json\b/i)
      end
    end.must_raise Roda::RodaError
  end

  it "supports :error_handler option" do
    app(:bare) do
      plugin(:json_parser, :error_handler=>proc{|r| r.halt [401, {}, ['bad']]}, :content_type_regexp=>/\Aapplication\/json\b/i)
      route do |r|
        r.params['a']['b'].to_s
      end
    end
    req('rack.input'=>rack_input('{"a":{"b":1}'), 'CONTENT_TYPE'=>'application/json', 'REQUEST_METHOD'=>'POST').must_equal [401, {}, ['bad']]
  end

  it "works with bare POST" do
    app(:bare) do
      plugin(:json_parser, :error_handler=>proc{|r| r.halt [401, {}, ['bad']]}, :content_type_regexp=>/\Aapplication\/json\b/i)
      route do |r|
        (r.POST['a']['b'] + r.POST['a']['c']).to_s
      end
    end
    body('rack.input'=>rack_input('{"a":{"b":1,"c":2}}'), 'CONTENT_TYPE'=>'application/json', 'REQUEST_METHOD'=>'POST').must_equal '3'
  end

  it "supports :parser option" do
    app(:bare) do
      plugin(:json_parser, :parser=>method(:eval), :content_type_regexp=>/\Aapplication\/json\b/i)
      route do |r|
        r.params['a']['b'].to_s
      end
    end
    body('rack.input'=>rack_input("{'a'=>{'b'=>1}}"), 'CONTENT_TYPE'=>'application/json', 'REQUEST_METHOD'=>'POST').must_equal '1'
  end

  it "supports :include_request option" do
    app(:bare) do
      plugin(:json_parser,
        :include_request => true,
        :parser => lambda{|s,r| {'a'=>s, 'b'=>r.path_info}},
        :content_type_regexp=>/\Aapplication\/json\b/i)
      route do |r|
        "#{r.params['a']}:#{r.params['b']}"
      end
    end
    body('rack.input'=>rack_input('{}'), 'CONTENT_TYPE'=>'application/json', 'REQUEST_METHOD'=>'POST').must_equal '{}:/'
  end

  it "supports resetting :include_request option to false" do
    app(:bare) do
      plugin :json_parser, :include_request => true, :content_type_regexp=>/\Aapplication\/json\b/i
      plugin :json_parser, :include_request => false
      route do |r|
        r.params['a']['b'].to_s
      end
    end
    body('rack.input'=>rack_input('{"a":{"b":1}}'), 'CONTENT_TYPE'=>'application/json', 'REQUEST_METHOD'=>'POST').must_equal '1'
  end
end

describe "json_parser plugin :parsers option" do
  def parsers_app(parsers, opts={}, &block)
    app(:bare) do
      plugin :json_parser,
        :content_type_regexp=>/\Aapplication\/(?:vnd\.[a-z]+\+)?json\b/i,
        :parsers=>parsers,
        **opts
      route(&block)
    end
  end

  it "uses the parser for the matching media type" do
    parsers_app(
      'application/json'=>proc{|s| JSON.parse(s)},
      'application/vnd.acme+json'=>proc{|s| {'vendor'=>'acme', 'raw'=>s}}
    ) do |r|
      "#{r.POST['a'].inspect}:#{r.POST['vendor'].inspect}:#{r.POST['raw'].inspect}"
    end
    body('rack.input'=>rack_input('{"a":1}'), 'CONTENT_TYPE'=>'application/json', 'REQUEST_METHOD'=>'POST').must_equal '1:nil:nil'
    body('rack.input'=>rack_input('{"a":1}'), 'CONTENT_TYPE'=>'application/vnd.acme+json', 'REQUEST_METHOD'=>'POST').must_equal 'nil:"acme":"{\"a\":1}"'
  end

  it "matches media types case insensitively and ignores media type parameters" do
    parsers_app(
      'application/vnd.acme+json'=>proc{|s| {'acme'=>true}}
    ) do |r|
      r.POST['acme'].inspect
    end
    body('rack.input'=>rack_input('{}'), 'CONTENT_TYPE'=>'Application/VND.Acme+JSON; charset=utf-8', 'REQUEST_METHOD'=>'POST').must_equal 'true'
  end

  it "normalizes the :parsers keys" do
    parsers_app(
      'Application/VND.Acme+JSON; charset=utf-8'=>proc{|s| {'acme'=>true}}
    ) do |r|
      r.POST['acme'].inspect
    end
    body('rack.input'=>rack_input('{}'), 'CONTENT_TYPE'=>'application/vnd.acme+json', 'REQUEST_METHOD'=>'POST').must_equal 'true'
  end

  it "raises UnsupportedMediaType for JSON media types with no parser" do
    parsers_app(
      'application/vnd.acme+json'=>proc{|s| {'acme'=>true}}
    ) do |r|
      r.POST.inspect
    end
    proc do
      body('rack.input'=>rack_input('{}'), 'CONTENT_TYPE'=>'application/vnd.other+json', 'REQUEST_METHOD'=>'POST')
    end.must_raise Roda::RodaPlugins::JsonParser::UnsupportedMediaType
  end

  it "raises UnsupportedMediaType for JSON media types with no parser when using :parser as well" do
    parsers_app(
      {'application/vnd.acme+json'=>proc{|s| {'acme'=>true}}},
      :parser=>proc{|s| JSON.parse(s)}
    ) do |r|
      r.POST.inspect
    end
    proc do
      body('rack.input'=>rack_input('{}'), 'CONTENT_TYPE'=>'application/json', 'REQUEST_METHOD'=>'POST')
    end.must_raise Roda::RodaPlugins::JsonParser::UnsupportedMediaType
  end

  it "raises UnsupportedMediaType when accessing params" do
    parsers_app('application/vnd.acme+json'=>proc{|s| {'acme'=>true}}) do |r|
      r.params.inspect
    end
    proc do
      body('rack.input'=>rack_input('{}'), 'CONTENT_TYPE'=>'application/vnd.other+json', 'REQUEST_METHOD'=>'POST')
    end.must_raise Roda::RodaPlugins::JsonParser::UnsupportedMediaType
  end

  it "is not handled by the invalid_request_body plugin" do
    app(:bare) do
      plugin :invalid_request_body, :empty_hash
      plugin :json_parser, :content_type_regexp=>/\Aapplication\/(?:vnd\.[a-z]+\+)?json\b/i,
        :parsers=>{'application/vnd.acme+json'=>proc{|s| {'acme'=>true}}}
      route{|r| r.POST.inspect}
    end
    proc do
      body('rack.input'=>rack_input('{}'), 'CONTENT_TYPE'=>'application/vnd.other+json', 'REQUEST_METHOD'=>'POST')
    end.must_raise Roda::RodaPlugins::JsonParser::UnsupportedMediaType
  end

  it "handles errors raised by the media type specific parser using the error handler" do
    parsers_app(
      {'application/vnd.acme+json'=>proc{|s| raise 'bad'}},
      :error_handler=>proc{|r| r.halt [422, {}, ['invalid']]}
    ) do |r|
      r.POST.inspect
    end
    req('rack.input'=>rack_input('{}'), 'CONTENT_TYPE'=>'application/vnd.acme+json', 'REQUEST_METHOD'=>'POST').must_equal [422, {}, ['invalid']]
  end

  it "does not raise for non-JSON content types" do
    parsers_app({'application/vnd.acme+json'=>proc{|s| {'acme'=>true}}}) do |r|
      r.params['a'].to_s
    end
    body('rack.input'=>rack_input('a=1'), 'CONTENT_TYPE'=>'application/x-www-form-urlencoded', 'REQUEST_METHOD'=>'POST').must_equal '1'
  end

  it "rejects non-Hash :parsers option" do
    proc do
      app(:bare) do
        plugin :json_parser, :content_type_regexp=>/\Aapplication\/json\b/i, :parsers=>[]
      end
    end.must_raise Roda::RodaError
  end

  it "rejects non-String :parsers keys" do
    proc do
      app(:bare) do
        plugin :json_parser, :content_type_regexp=>/\Aapplication\/json\b/i, :parsers=>{/application\/json/=>proc{}}
      end
    end.must_raise Roda::RodaError
  end

  it "rejects empty :parsers keys" do
    proc do
      app(:bare) do
        plugin :json_parser, :content_type_regexp=>/\Aapplication\/json\b/i, :parsers=>{''=>proc{}}
      end
    end.must_raise Roda::RodaError
  end

  it "rejects :parsers values that do not respond to call" do
    proc do
      app(:bare) do
        plugin :json_parser, :content_type_regexp=>/\Aapplication\/json\b/i, :parsers=>{'application/vnd.acme+json'=>1}
      end
    end.must_raise Roda::RodaError
  end

  it "rejects :parsers entries that conflict after normalization" do
    proc do
      app(:bare) do
        plugin :json_parser, :content_type_regexp=>/\Aapplication\/json\b/i,
          :parsers=>{'application/vnd.acme+json'=>proc{}, 'Application/VND.Acme+JSON; charset=utf-8'=>proc{}}
      end
    end.must_raise Roda::RodaError
  end

  it "supports inheriting :parsers from a parent application" do
    app(:bare) do
      plugin :json_parser, :content_type_regexp=>/\Aapplication\/(?:vnd\.[a-z]+\+)?json\b/i,
        :parsers=>{'application/vnd.acme+json'=>proc{|s| {'acme'=>true}}}
      route{|r| r.POST['acme'].inspect}
    end
    parent = @app
    child = Class.new(parent)
    child.route{|r| r.POST['acme'].inspect}

    body('rack.input'=>rack_input('{}'), 'CONTENT_TYPE'=>'application/vnd.acme+json', 'REQUEST_METHOD'=>'POST').must_equal 'true'
    b = _req(child, req_env('rack.input'=>rack_input('{}'), 'CONTENT_TYPE'=>'application/vnd.acme+json', 'REQUEST_METHOD'=>'POST'))[2]
    b.to_a.join.must_equal 'true'
  end
end

describe "json_parser plugin introspection" do
  it "reports the normalized media type" do
    app(:bare) do
      plugin :json_parser, :content_type_regexp=>/\Aapplication\/(?:vnd\.[a-z]+\+)?json\b/i
      route{|r| r.json_parser_media_type.inspect}
    end
    body('rack.input'=>rack_input('{}'), 'CONTENT_TYPE'=>'Application/Vnd.Acme+JSON; charset=utf-8', 'REQUEST_METHOD'=>'POST').must_equal '"application/vnd.acme+json"'
  end

  it "returns nil as the media type when there is no content type" do
    app(:bare) do
      plugin :json_parser, :content_type_regexp=>/\Aapplication\/json\b/i
      route{|r| r.json_parser_media_type.inspect}
    end
    body('REQUEST_METHOD'=>'GET').must_equal 'nil'
  end

  it "reports nil result before parsing" do
    app(:bare) do
      plugin :json_parser, :content_type_regexp=>/\Aapplication\/json\b/i
      route{|r| r.json_parser_result.inspect}
    end
    body('rack.input'=>rack_input('{"a":1}'), 'CONTENT_TYPE'=>'application/json', 'REQUEST_METHOD'=>'POST').must_equal 'nil'
  end

  it "reports :parsed result, with no error" do
    app(:bare) do
      plugin :json_parser, :content_type_regexp=>/\Aapplication\/json\b/i
      route{|r| r.POST; "#{r.json_parser_result.inspect}:#{r.json_parser_error.inspect}"}
    end
    body('rack.input'=>rack_input('{"a":1}'), 'CONTENT_TYPE'=>'application/json', 'REQUEST_METHOD'=>'POST').must_equal ':parsed:nil'
  end

  it "reports :empty result for an empty body" do
    app(:bare) do
      plugin :json_parser, :content_type_regexp=>/\Aapplication\/json\b/i
      route{|r| r.POST; r.json_parser_result.inspect}
    end
    body('rack.input'=>rack_input(''), 'CONTENT_TYPE'=>'application/json', 'REQUEST_METHOD'=>'POST').must_equal ':empty'
  end

  it "reports :none result for non-JSON content types" do
    app(:bare) do
      plugin :json_parser, :content_type_regexp=>/\Aapplication\/json\b/i
      route{|r| r.POST; r.json_parser_result.inspect}
    end
    body('rack.input'=>rack_input('a=1'), 'CONTENT_TYPE'=>'application/x-www-form-urlencoded', 'REQUEST_METHOD'=>'POST').must_equal ':none'
  end

  it "reports :invalid result and the parser exception" do
    error = nil
    app(:bare) do
      plugin :json_parser,
        :content_type_regexp=>/\Aapplication\/json\b/i,
        :error_handler=>proc{|r| error = r; r.halt [400, {}, []]}
      route do |r|
        r.POST
      end
    end
    req('rack.input'=>rack_input('{'), 'CONTENT_TYPE'=>'application/json', 'REQUEST_METHOD'=>'POST').must_equal [400, {}, []]
    error.json_parser_result.must_equal :invalid
    error.json_parser_error.must_be_kind_of JSON::ParserError
  end

  it "reports :unsupported_media_type for JSON media types with no parser" do
    app(:bare) do
      plugin :json_parser, :content_type_regexp=>/\Aapplication\/(?:vnd\.[a-z]+\+)?json\b/i,
        :parsers=>{'application/vnd.acme+json'=>proc{|s| JSON.parse(s)}}
      route do |r|
        r.POST
      rescue Roda::RodaPlugins::JsonParser::UnsupportedMediaType
        "#{r.json_parser_result.inspect}:#{r.json_parser_error.inspect}"
      end
    end
    body('rack.input'=>rack_input('{}'), 'CONTENT_TYPE'=>'application/vnd.other+json', 'REQUEST_METHOD'=>'POST').must_equal ':unsupported_media_type:nil'
  end

  it "reports the parser that would be used for the request media type" do
    acme = proc{|s| {'acme'=>true}}
    app(:bare) do
      plugin :json_parser, :content_type_regexp=>/\Aapplication\/(?:vnd\.[a-z]+\+)?json\b/i,
        :parsers=>{'application/vnd.acme+json'=>acme}
      route{|r| (r.json_parser_parser == acme).to_s}
    end
    body('rack.input'=>rack_input('{}'), 'CONTENT_TYPE'=>'application/vnd.acme+json', 'REQUEST_METHOD'=>'POST').must_equal 'true'
  end

  it "reports nil parser for an unsupported media type when :parsers is used" do
    app(:bare) do
      plugin :json_parser, :content_type_regexp=>/\Aapplication\/(?:vnd\.[a-z]+\+)?json\b/i,
        :parsers=>{'application/vnd.acme+json'=>proc{|s| JSON.parse(s)}}
      route{|r| r.json_parser_parser.inspect}
    end
    body('rack.input'=>rack_input('{}'), 'CONTENT_TYPE'=>'application/vnd.other+json', 'REQUEST_METHOD'=>'POST').must_equal 'nil'
  end

  it "reports nil parser for non-JSON media types" do
    app(:bare) do
      plugin :json_parser, :content_type_regexp=>/\Aapplication\/json\b/i
      route{|r| r.json_parser_parser.inspect}
    end
    body('rack.input'=>rack_input('a=1'), 'CONTENT_TYPE'=>'application/x-www-form-urlencoded', 'REQUEST_METHOD'=>'POST').must_equal 'nil'
  end

  it "reports a distinct result per application, visible from the subapplication" do
    sub_app = Class.new(Roda) do
      plugin :json_parser, :content_type_regexp=>/\Aapplication\/(?:vnd\.[a-z]+\+)?json\b/i,
        :parsers=>{'application/vnd.other+json'=>proc{|s| {'other'=>true}}}
      route do |r|
        # The subapplication uses its own parsers, so it considers the acme
        # media type unsupported even though the parent parsed it.
        r.POST
      rescue Roda::RodaPlugins::JsonParser::UnsupportedMediaType
        "#{r.json_parser_result.inspect}:#{r.env['parent.json_parser_result'].inspect}"
      end
    end

    app(:bare) do
      plugin :json_parser, :content_type_regexp=>/\Aapplication\/(?:vnd\.[a-z]+\+)?json\b/i,
        :parsers=>{'application/vnd.acme+json'=>proc{|s| {'acme'=>true}}}
      route do |r|
        r.on 'sub' do
          r.POST
          r.env['parent.json_parser_result'] = r.json_parser_result
          r.run sub_app
        end
      end
    end

    body('/sub', 'rack.input'=>rack_input('{}'), 'CONTENT_TYPE'=>'application/vnd.acme+json', 'REQUEST_METHOD'=>'POST').must_equal ':unsupported_media_type::parsed'
  end

  it "does not call the error handler twice when the error handler halts" do
    calls = 0
    app(:bare) do
      plugin :json_parser,
        :content_type_regexp=>/\Aapplication\/json\b/i,
        :error_handler=>proc{|r| calls += 1; r.halt [400, {}, []]}
      route{|r| r.POST; r.POST}
    end
    req('rack.input'=>rack_input('{'), 'CONTENT_TYPE'=>'application/json', 'REQUEST_METHOD'=>'POST').must_equal [400, {}, []]
    calls.must_equal 1
  end

  it "does not call the error handler twice when the error handler raises" do
    calls = 0
    app(:bare) do
      plugin :json_parser,
        :content_type_regexp=>/\Aapplication\/json\b/i,
        :error_handler=>proc{|r| calls += 1; raise 'bad'}
      route{|r| r.POST}
    end
    proc do
      req('rack.input'=>rack_input('{'), 'CONTENT_TYPE'=>'application/json', 'REQUEST_METHOD'=>'POST')
    end.must_raise RuntimeError
    calls.must_equal 1
  end

  it "calls the error handler at most once per application class for an invalid body" do
    calls = 0
    app(:bare) do
      plugin :json_parser,
        :content_type_regexp=>/\Aapplication\/json\b/i,
        :error_handler=>proc{|r| calls += 1}
      route{|r| "#{r.POST.inspect}:#{r.POST.inspect}"}
    end
    body('rack.input'=>rack_input('{'), 'CONTENT_TYPE'=>'application/json', 'REQUEST_METHOD'=>'POST').must_equal 'nil:nil'
    calls.must_equal 1
  end

  it "returns the :parsed result with the parsed params wrapped using :wrap" do
    app(:bare) do
      plugin :json_parser, :content_type_regexp=>/\Aapplication\/json\b/i, :wrap=>:unless_hash
      route{|r| r.POST; "#{r.json_parser_result}:#{r.POST['_json'][1]}"}
    end
    body('rack.input'=>rack_input('[true, 2]'), 'CONTENT_TYPE'=>'application/json', 'REQUEST_METHOD'=>'POST').must_equal 'parsed:2'
  end

  it "supports json_parser_parser_for at the class level" do
    acme = proc{|s| {'acme'=>true}}
    app(:bare) do
      plugin :json_parser, :content_type_regexp=>/\Aapplication\/(?:vnd\.[a-z]+\+)?json\b/i,
        :parsers=>{'application/vnd.acme+json'=>acme}
      route{|r| ''}
    end
    @app.json_parser_parser_for('application/vnd.acme+json').must_be_same_as acme
    @app.json_parser_parser_for('application/vnd.other+json').must_be_nil
    @app.json_parser_parsers.keys.must_equal ['application/vnd.acme+json']
  end

  it "returns the general parser from json_parser_parser_for without :parsers" do
    app(:bare) do
      plugin :json_parser, :content_type_regexp=>/\Aapplication\/json\b/i
      route{|r| ''}
    end
    @app.json_parser_parser_for('application/whatever').must_equal JSON.method(:parse)
  end
end

describe "json_parser plugin single input stream" do
  def counting_input(str)
    input = Class.new(StringIO) do
      attr_reader :read_count, :rewind_count
      def read(*args)
        @read_count = (@read_count || 0) + 1
        super
      end
      def rewind
        @rewind_count = (@rewind_count || 0) + 1
        super
      end
    end
    input.new(str.dup.force_encoding('BINARY'))
  end

  # An input stream that can only be read once and cannot be rewound, as
  # allowed by Rack 3.
  def one_shot_input(str)
    Class.new do
      attr_reader :read_count
      def initialize(str)
        @str = str.dup.force_encoding('BINARY')
        @read_count = 0
      end
      def read(*args)
        @read_count += 1
        raise IOError, "input already read" if @read_count > 1
        str = @str
        @str = ''.b
        str
      end
      def rewind
        raise IOError, "input is not rewindable"
      end
      def gets
        raise IOError, "input is not line based"
      end
      def each
        raise IOError, "input is not line based"
      end
      def close
      end
    end.new(str)
  end

  it "works with a one-shot input stream for the default parser" do
    input = one_shot_input('{"a":{"b":1}}')
    app(:bare) do
      plugin :json_parser, :content_type_regexp=>/\Aapplication\/json\b/i
      route do |r|
        "#{r.params['a']['b']}:#{r.POST['a']['b']}"
      end
    end
    body('rack.input'=>input, 'CONTENT_TYPE'=>'application/json', 'REQUEST_METHOD'=>'POST').must_equal '1:1'
    input.read_count.must_equal 1
  end

  it "works with a one-shot input stream for a media type specific parser" do
    input = one_shot_input('{"a":1}')
    app(:bare) do
      plugin :json_parser,
        :content_type_regexp=>/\Aapplication\/(?:vnd\.[a-z]+\+)?json\b/i,
        :parsers=>{'application/vnd.acme+json'=>proc{|s| {'acme'=>s}}}
      route do |r|
        "#{r.POST['acme']}:#{r.json_parser_result}"
      end
    end
    body('rack.input'=>input, 'CONTENT_TYPE'=>'application/vnd.acme+json', 'REQUEST_METHOD'=>'POST').must_equal '{"a":1}:parsed'
    input.read_count.must_equal 1
  end

  it "works with a one-shot input stream for an unsupported media type" do
    input = one_shot_input('{"a":1}')
    app(:bare) do
      plugin :json_parser,
        :content_type_regexp=>/\Aapplication\/(?:vnd\.[a-z]+\+)?json\b/i,
        :parsers=>{'application/vnd.acme+json'=>proc{|s| JSON.parse(s)}}
      route do |r|
        r.POST
      rescue Roda::RodaPlugins::JsonParser::UnsupportedMediaType
        "#{r.json_parser_result}:#{r.json_parser_media_type}"
      end
    end
    body('rack.input'=>input, 'CONTENT_TYPE'=>'application/vnd.other+json', 'REQUEST_METHOD'=>'POST').must_equal 'unsupported_media_type:application/vnd.other+json'
    # The body is not read when there is no parser for the media type.
    input.read_count.must_equal 0
  end

  it "sets env['roda.json_params'] for backwards compatibility" do
    app(:bare) do
      plugin :json_parser, :content_type_regexp=>/\Aapplication\/json\b/i
      route do |r|
        r.POST
        r.env['roda.json_params']['a'].to_s
      end
    end
    body('rack.input'=>rack_input('{"a":1}'), 'CONTENT_TYPE'=>'application/json', 'REQUEST_METHOD'=>'POST').must_equal '1'
  end

  it "reads the original input stream only once for multiple POST/params calls" do
    input = counting_input('{"a":1}')
    app(:bare) do
      plugin :json_parser, :content_type_regexp=>/\Aapplication\/json\b/i
      route do |r|
        "#{r.POST['a']}:#{r.params['a']}:#{r.POST['a']}"
      end
    end
    body('rack.input'=>input, 'CONTENT_TYPE'=>'application/json', 'REQUEST_METHOD'=>'POST').must_equal '1:1:1'
    input.read_count.must_equal 1
  end

  it "replaces rack.input with a rewindable StringIO containing the body" do
    input = counting_input('{"a":1}')
    app(:bare) do
      plugin :json_parser, :content_type_regexp=>/\Aapplication\/json\b/i
      route do |r|
        r.POST
        r.env['rack.input'].read
      end
    end
    body('rack.input'=>input, 'CONTENT_TYPE'=>'application/json', 'REQUEST_METHOD'=>'POST').must_equal '{"a":1}'
    input.read_count.must_equal 1
  end

  it "does not read the original input stream for an empty content type" do
    input = counting_input('{"a":1}')
    app(:bare) do
      plugin :json_parser, :content_type_regexp=>/\Aapplication\/json\b/i
      route{|r| r.json_parser_result.inspect}
    end
    body('rack.input'=>input, 'REQUEST_METHOD'=>'GET').must_equal 'nil'
    input.read_count.must_be_nil
  end

  it "does not read the original input stream a second time when a nested app parses the body" do
    input = counting_input('{"a":1}')

    sub_app = Class.new(Roda) do
      plugin :json_parser, :content_type_regexp=>/\Aapplication\/json\b/i,
        :parsers=>{'application/json'=>proc{|s| {'sub'=>s}}}
      route{|r| r.POST['sub']}
    end

    app(:bare) do
      plugin :json_parser, :content_type_regexp=>/\Aapplication\/json\b/i
      route do |r|
        r.on 'sub' do
          r.POST
          r.run sub_app
        end
      end
    end

    body('/sub', 'rack.input'=>input, 'CONTENT_TYPE'=>'application/json', 'REQUEST_METHOD'=>'POST').must_equal '{"a":1}'
    input.read_count.must_equal 1
  end

  it "uses the same cached body when a later app has a different parser" do
    input = counting_input('{"a":1}')

    sub_app = Class.new(Roda) do
      plugin :json_parser,
        :content_type_regexp=>/\Aapplication\/json\b/i,
        :parsers=>{'application/json'=>proc{|s| {'sub'=>s.upcase}}}
      route{|r| r.POST['sub']}
    end

    app(:bare) do
      plugin :json_parser, :content_type_regexp=>/\Aapplication\/json\b/i
      route do |r|
        r.on 'sub' do
          r.POST
          r.run sub_app
        end
      end
    end

    body('/sub', 'rack.input'=>input, 'CONTENT_TYPE'=>'application/json', 'REQUEST_METHOD'=>'POST').must_equal '{"A":1}'
    input.read_count.must_equal 1
  end

  it "gives each app its own parse result and parser when applications have different parsers" do
    input = counting_input('{"a":1}')

    sub_app = Class.new(Roda) do
      plugin :json_parser,
        :content_type_regexp=>/\Aapplication\/json\b/i,
        :parsers=>{'application/json'=>proc{|s| {'sub'=>true}}}
      route do |r|
        sub_params = r.POST
        sub_result = r.json_parser_result
        # The subapplication can see the parent application's result, which
        # was stored per application class.
        parent_result = r.env['parent.json_parser_result']
        "#{sub_params['sub']}:#{sub_result}:#{parent_result}"
      end
    end

    app(:bare) do
      plugin :json_parser, :content_type_regexp=>/\Aapplication\/json\b/i
      route do |r|
        r.on 'sub' do
          r.env['parent.json_parser_result'] = r.POST && r.json_parser_result
          r.run sub_app
        end
      end
    end

    body('/sub', 'rack.input'=>input, 'CONTENT_TYPE'=>'application/json', 'REQUEST_METHOD'=>'POST').must_equal 'true:parsed:parsed'
    input.read_count.must_equal 1
  end
end

