require_relative "../spec_helper"

describe 'response.cache_control' do
  it 'sets the Cache-Control header' do
    app(:caching) do |r|
      response.cache_control :public=>true, :no_cache=>true, :max_age => 60
    end
    header(RodaResponseHeaders::CACHE_CONTROL).split(', ').sort.must_equal ['max-age=60', 'no-cache', 'public']
  end

  it 'does not add a Cache-Control header if it would be empty' do
    app(:caching) do |r|
      response.cache_control({})
    end
    header(RodaResponseHeaders::CACHE_CONTROL).must_be_nil
  end

  it 'skips Cache-Control nil parameters' do
    app(:caching) do |r|
      response.cache_control(:max_age=>nil)
    end
    header(RodaResponseHeaders::CACHE_CONTROL).must_be_nil
  end
end

describe 'response.expires' do
  it 'sets the Cache-Control and Expires header' do
    app(:caching) do |r|
      response.expires 60, :public=>true, :no_cache=>true
    end
    header(RodaResponseHeaders::CACHE_CONTROL).split(', ').sort.must_equal ['max-age=60', 'no-cache', 'public']
    ((Time.httpdate(header(RodaResponseHeaders::EXPIRES)) - Time.now).round - 60).abs.must_be :<=, 1
  end

  it 'can be called with only one argument' do
    app(:caching) do |r|
      response.expires 60
    end
    header(RodaResponseHeaders::CACHE_CONTROL).split(', ').sort.must_equal ['max-age=60']
    ((Time.httpdate(header(RodaResponseHeaders::EXPIRES)) - Time.now).round - 60).abs.must_be :<=, 1
  end
end

describe 'response.finish' do
  it 'removes Content-Type and Content-Length for 304 responses' do
    app(:caching) do |r|
      response.status = 304
      nil
    end
    header(RodaResponseHeaders::CONTENT_TYPE).must_be_nil
    header(RodaResponseHeaders::CONTENT_LENGTH).must_be_nil
  end

  it 'does not change non-304 responses' do
    app(:caching) do |r|
      response.status = 200
      nil
    end
    header(RodaResponseHeaders::CONTENT_TYPE).must_equal 'text/html'
    header(RodaResponseHeaders::CONTENT_LENGTH).must_equal '0'
  end
end

describe 'request.last_modified' do
  it 'ignores nil' do
    app(:caching) do |r|
      r.last_modified nil
    end
    header(RodaResponseHeaders::LAST_MODIFIED).must_be_nil
  end

  it 'does not change a status other than 200' do
    app(:caching) do |r|
      response.status = 201
      r.last_modified Time.now
    end
    status.must_equal 201
    status('HTTP_IF_MODIFIED_SINCE' => 'Sun, 26 Sep 2030 23:43:52 GMT').must_equal 201
    status('HTTP_IF_MODIFIED_SINCE' => 'Sun, 26 Sep 2000 23:43:52 GMT').must_equal 201
  end
end

describe 'request.last_modified' do
  def res(a={})
    s, h, b = req(a)
    h[RodaResponseHeaders::LAST_MODIFIED].must_equal @last_modified.httpdate
    [s, b.join]
  end

  before(:all) do
    lm = @last_modified = Time.now
    app(:caching) do |r|
      r.last_modified lm
      'ok'
    end
  end

  it 'just sets Last-Modified if no If-Modified-Since header' do
    res.must_equal [200, 'ok']
  end

  it 'just sets Last-Modified if bogus If-Modified-Since header' do
    res('HTTP_IF_MODIFIED_SINCE' => 'a really weird date').must_equal [200, 'ok']
  end

  it 'just sets Last-Modified if modified since If-Modified-Since header' do
    res('HTTP_IF_MODIFIED_SINCE' => (@last_modified - 1).httpdate).must_equal [200, 'ok']
  end

  it 'sets Last-Modified and returns 304 if modified on If-Modified-Since header' do
    res('HTTP_IF_MODIFIED_SINCE' => @last_modified.httpdate).must_equal [304, '']
  end

  it 'sets Last-Modified and returns 304 if modified before If-Modified-Since header' do
    res('HTTP_IF_MODIFIED_SINCE' => (@last_modified + 1).httpdate).must_equal [304, '']
  end

  it 'sets Last-Modified if If-None-Match header present' do
    res('HTTP_IF_NONE_MATCH' => '*', 'HTTP_IF_MODIFIED_SINCE' => (@last_modified + 1).httpdate).must_equal [200, 'ok']
  end

  it 'sets Last-Modified if modified before If-Unmodified-Since header' do
    res('HTTP_IF_UNMODIFIED_SINCE' => (@last_modified + 1).httpdate).must_equal [200, 'ok']
  end

  it 'sets Last-Modified if modified on If-Unmodified-Since header' do
    res('HTTP_IF_UNMODIFIED_SINCE' => @last_modified.httpdate).must_equal [200, 'ok']
  end

  it 'sets Last-Modified and returns 412 if modified after If-Unmodified-Since header' do
    res('HTTP_IF_UNMODIFIED_SINCE' => (@last_modified - 1).httpdate).must_equal [412, '']
    res('HTTP_IF_UNMODIFIED_SINCE' => (@last_modified - 1).httpdate, 'HTTP_IF_NONE_MATCH' => '*').must_equal [412, '']
    res('HTTP_IF_UNMODIFIED_SINCE' => (@last_modified - 1).httpdate, 'HTTP_IF_MODIFIED_SINCE' => (@last_modified + 1).httpdate).must_equal [412, '']
  end
end

describe 'request.etag' do
  before(:all) do
    app(:caching) do |r|
      r.is "pos" do
        r.etag('foo', {weak: true})
        'ok'
      end
      r.is "extra_kw" do
        r.etag('foo', bad: true)
        'ok'
      end
      r.is "" do
        response.status = r.env['status'].to_i if r.env['status']
        etag_opts = {}
        etag_opts[:new_resource] = r.env['new_resource'] == 'true' if r.env.has_key?('new_resource')
        etag_opts[:weak] = r.env['weak'] == 'true' if r.env.has_key?('weak')
        r.etag 'foo', **etag_opts
        'ok'
      end
    end
  end

  def res(a={})
    a['status'] = a['status'].to_s if a['status']
    s, h, b = req(a)
    h[RodaResponseHeaders::ETAG].must_equal '"foo"'
    [s, b.join]
  end

  it 'uses a weak etag with the :weak option' do
    header(RodaResponseHeaders::ETAG, 'weak'=>'true').must_equal 'W/"foo"'
  end

  deprecated "warns for positional hash usage and unexpected keywords" do
    body('/pos').must_equal 'ok'
    body('/extra_kw').must_equal 'ok'
  end

  describe 'for GET requests' do
    it "sets etag if no If-None-Match" do
      res.must_equal [200, 'ok']
    end

    it "sets etag and returns 304 if If-None-Match is *" do
      res('HTTP_IF_NONE_MATCH' => '*').must_equal [304, '']
    end

    it "sets etag and if If-None-Match is * and it is a new resource" do
      res('HTTP_IF_NONE_MATCH' => '*', 'new_resource'=>'true').must_equal [200, 'ok']
    end

    it "sets etag and returns 304 if If-None-Match is etag" do
      res('HTTP_IF_NONE_MATCH' => '"foo"').must_equal [304, '']
    end

    it "sets etag and returns 304 if If-None-Match includes etag" do
      res('HTTP_IF_NONE_MATCH' => '"bar", "foo"').must_equal [304, '']
    end

    it "sets etag and returns 304 if If-None-Match includes the weak form of the etag" do
      res('HTTP_IF_NONE_MATCH' => 'W/"foo"').must_equal [304, '']
      res('HTTP_IF_NONE_MATCH' => '"bar", W/"foo"').must_equal [304, '']
    end

    it "returns 304 if If-None-Match includes the strong form of a weak etag" do
      status('HTTP_IF_NONE_MATCH' => '"foo"', 'weak'=>'true').must_equal 304
      status('HTTP_IF_NONE_MATCH' => 'W/"bar", "foo"', 'weak'=>'true').must_equal 304
      status('HTTP_IF_NONE_MATCH' => 'W/"bar"', 'weak'=>'true').must_equal 200
    end

    it "sets etag if If-None-Match does not include etag" do
      res('HTTP_IF_NONE_MATCH' => '"bar", "baz"').must_equal [200, 'ok']
    end

    it "sets etag and does not change status code if status code set and not 2xx or 304 if If-None-Match is etag" do
      res('HTTP_IF_NONE_MATCH' => '"foo"', 'status'=>499).must_equal [499, 'ok']
    end

    it "sets etag and returns 304 if status code set to 2xx if If-None-Match is etag" do
      res('HTTP_IF_NONE_MATCH' => '"foo"', 'status'=>201).must_equal [304, '']
    end

    it "sets etag and returns 304 if status code is already 304 if If-None-Match is etag" do
      res('HTTP_IF_NONE_MATCH' => '"foo"', 'status'=>304).must_equal [304, '']
    end

    it "sets etag if If-Match is *" do
      res('HTTP_IF_MATCH' => '*').must_equal [200, 'ok']
    end

    it "sets etag if If-Match is etag" do
      res('HTTP_IF_MATCH' => '"foo"').must_equal [200, 'ok']
    end

    it "sets etag if If-Match includes etag" do
      res('HTTP_IF_MATCH' => '"bar", "foo"').must_equal [200, 'ok']
    end

    it "sets etag and returns 412 if If-Match is given a weak etag" do
      status('HTTP_IF_MATCH' => 'W/"foo"', 'weak'=>'true').must_equal 412
    end

    it "sets etag and returns 412 if If-Match is * for new resources" do
      res('HTTP_IF_MATCH' => '*', 'new_resource'=>'true').must_equal [412, '']
    end

    it "sets etag if If-Match does not include etag" do
      res('HTTP_IF_MATCH' => '"bar", "baz"', 'new_resource'=>'true').must_equal [412, '']
    end
  end

  describe 'for PUT requests' do
    def res(a={})
      super(a.merge('REQUEST_METHOD'=>'PUT'))
    end

    it "sets etag if no If-None-Match" do
      res.must_equal [200, 'ok']
    end

    it "sets etag and returns 412 if If-None-Match is *" do
      res('HTTP_IF_NONE_MATCH' => '*').must_equal [412, '']
    end

    it "sets etag and if If-None-Match is * and it is a new resource" do
      res('HTTP_IF_NONE_MATCH' => '*', 'new_resource'=>'true').must_equal [200, 'ok']
    end

    it "sets etag and returns 412 if If-None-Match is etag" do
      res('HTTP_IF_NONE_MATCH' => '"foo"').must_equal [412, '']
    end

    it "sets etag and returns 412 if If-None-Match includes etag" do
      res('HTTP_IF_NONE_MATCH' => '"bar", "foo"').must_equal [412, '']
    end

    it "sets etag if If-None-Match does not include etag" do
      res('HTTP_IF_NONE_MATCH' => '"bar", "baz"').must_equal [200, 'ok']
    end

    it "sets etag and does not change status code if status code set and not 2xx or 304 if If-None-Match is etag" do
      res('HTTP_IF_NONE_MATCH' => '"foo"', 'status'=>499).must_equal [499, 'ok']
    end

    it "sets etag and returns 304 if status code set to 2xx if If-None-Match is etag" do
      res('HTTP_IF_NONE_MATCH' => '"foo"', 'status'=>201).must_equal [412, '']
    end

    it "sets etag and returns 304 if status code is already 304 if If-None-Match is etag" do
      res('HTTP_IF_NONE_MATCH' => '"foo"', 'status'=>304).must_equal [412, '']
    end

    it "sets etag if If-Match is *" do
      res('HTTP_IF_MATCH' => '*').must_equal [200, 'ok']
    end

    it "sets etag if If-Match is etag" do
      res('HTTP_IF_MATCH' => '"foo"').must_equal [200, 'ok']
    end

    it "sets etag if If-Match includes etag" do
      res('HTTP_IF_MATCH' => '"bar", "foo"').must_equal [200, 'ok']
    end

    it "sets etag and returns 412 if If-Match is * for new resources" do
      res('HTTP_IF_MATCH' => '*', 'new_resource'=>'true').must_equal [412, '']
    end

    it "sets etag if If-Match does not include etag" do
      res('HTTP_IF_MATCH' => '"bar", "baz"', 'new_resource'=>'true').must_equal [412, '']
    end
  end

  describe 'for POST requests' do
    def res(a={})
      super(a.merge('REQUEST_METHOD'=>'POST'))
    end

    it "sets etag if no If-None-Match" do
      res.must_equal [200, 'ok']
    end

    it "sets etag and returns 412 if If-None-Match is * and it is not a new resource" do
      res('HTTP_IF_NONE_MATCH' => '*', 'new_resource'=>'false').must_equal [412, '']
    end

    it "sets etag and if If-None-Match is *" do
      res('HTTP_IF_NONE_MATCH' => '*').must_equal [200, 'ok']
    end

    it "sets etag and returns 412 if If-None-Match is etag" do
      res('HTTP_IF_NONE_MATCH' => '"foo"').must_equal [412, '']
    end

    it "sets etag and returns 412 if If-None-Match includes etag" do
      res('HTTP_IF_NONE_MATCH' => '"bar", "foo"').must_equal [412, '']
    end

    it "sets etag if If-None-Match does not include etag" do
      res('HTTP_IF_NONE_MATCH' => '"bar", "baz"').must_equal [200, 'ok']
    end

    it "sets etag and does not change status code if status code set and not 2xx or 304 if If-None-Match is etag" do
      res('HTTP_IF_NONE_MATCH' => '"foo"', 'status'=>499).must_equal [499, 'ok']
    end

    it "sets etag and returns 304 if status code set to 2xx if If-None-Match is etag" do
      res('HTTP_IF_NONE_MATCH' => '"foo"', 'status'=>201).must_equal [412, '']
    end

    it "sets etag and returns 304 if status code is already 304 if If-None-Match is etag" do
      res('HTTP_IF_NONE_MATCH' => '"foo"', 'status'=>304).must_equal [412, '']
    end

    it "sets etag if If-Match is * and this is not a new resource" do
      res('HTTP_IF_MATCH' => '*', 'new_resource'=>'false').must_equal [200, 'ok']
    end

    it "sets etag if If-Match is etag" do
      res('HTTP_IF_MATCH' => '"foo"').must_equal [200, 'ok']
    end

    it "sets etag if If-Match includes etag" do
      res('HTTP_IF_MATCH' => '"bar", "foo"').must_equal [200, 'ok']
    end

    it "sets etag and returns 412 if If-Match is * for new resources" do
      res('HTTP_IF_MATCH' => '*').must_equal [412, '']
    end

    it "sets etag if If-Match does not include etag" do
      res('HTTP_IF_MATCH' => '"bar", "baz"', 'new_resource'=>'true').must_equal [412, '']
    end
  end
end

describe 'request.preconditions' do
  before(:all) do
    @precondition_checks = []
    checks = @precondition_checks
    lm = @precondition_last_modified = Time.now
    app(:caching) do |r|
      r.is "" do
        checks << r.request_method
        r.preconditions(etag: 'foo', last_modified: lm)
        checks << :continued
        'ok'
      end

      r.is 'etag_only' do
        checks << r.request_method
        r.preconditions(etag: 'foo')
        checks << :continued
        'ok'
      end

      r.is 'last_modified_only' do
        checks << r.request_method
        r.preconditions(last_modified: lm)
        checks << :continued
        'ok'
      end

      r.is 'weak' do
        checks << r.request_method
        r.preconditions(etag: 'foo', last_modified: lm, weak: true)
        checks << :continued
        'ok'
      end

      r.is 'no_values' do
        r.preconditions
        'ok'
      end

      r.is 'preset_status' do
        checks << r.request_method
        response.status = r.env['status'].to_i
        r.preconditions(etag: 'foo', last_modified: lm)
        checks << :continued
        'ok'
      end
    end
  end

  before do
    @precondition_checks.clear
  end

  def res(env={})
    s, _, b = req(env)
    [s, b.join]
  end

  it 'sets both ETag and Last-Modified headers' do
    s, h, b = req
    s.must_equal 200
    h[RodaResponseHeaders::ETAG].must_equal '"foo"'
    h[RodaResponseHeaders::LAST_MODIFIED].must_equal @precondition_last_modified.httpdate
    b.join.must_equal 'ok'
  end

  it 'ignores nil values' do
    res('PATH_INFO'=>'/no_values').must_equal [200, 'ok']
  end

  describe 'for GET requests' do
    it 'continues processing when no preconditions are given' do
      res.must_equal [200, 'ok']
      @precondition_checks.must_equal ['GET', :continued]
    end

    it 'halts with 304 if If-None-Match matches' do
      s, h, b = req('HTTP_IF_NONE_MATCH'=>'"foo"')
      [s, b.join].must_equal [304, '']
      h[RodaResponseHeaders::ETAG].must_equal '"foo"'
      h[RodaResponseHeaders::LAST_MODIFIED].must_equal @precondition_last_modified.httpdate
      @precondition_checks.must_equal ['GET']
    end

    it 'halts with 304 if If-None-Match matches using a weak comparison' do
      res('HTTP_IF_NONE_MATCH'=>'W/"foo"').must_equal [304, '']
      @precondition_checks.must_equal ['GET']
    end

    it 'continues if If-None-Match does not match' do
      res('HTTP_IF_NONE_MATCH'=>'"bar"').must_equal [200, 'ok']
      @precondition_checks.must_equal ['GET', :continued]
    end

    it 'halts with 304 if If-Modified-Since is not before Last-Modified' do
      res('HTTP_IF_MODIFIED_SINCE'=>@precondition_last_modified.httpdate).must_equal [304, '']
      res('HTTP_IF_MODIFIED_SINCE'=>(@precondition_last_modified + 1).httpdate).must_equal [304, '']
      @precondition_checks.must_equal ['GET', 'GET']
    end

    it 'continues processing if If-Modified-Since is before Last-Modified' do
      res('HTTP_IF_MODIFIED_SINCE'=>(@precondition_last_modified - 1).httpdate).must_equal [200, 'ok']
      @precondition_checks.must_equal ['GET', :continued]
    end

    it 'ignores If-Modified-Since if If-None-Match is present' do
      res('HTTP_IF_NONE_MATCH'=>'"bar"', 'HTTP_IF_MODIFIED_SINCE'=>(@precondition_last_modified + 1).httpdate).must_equal [200, 'ok']
      @precondition_checks.must_equal ['GET', :continued]
    end

    it 'halts with 412 if If-Match does not match' do
      res('HTTP_IF_MATCH'=>'"bar"').must_equal [412, '']
      @precondition_checks.must_equal ['GET']
    end

    it 'continues processing if If-Match matches' do
      res('HTTP_IF_MATCH'=>'*').must_equal [200, 'ok']
      res('HTTP_IF_MATCH'=>'"bar", "foo"').must_equal [200, 'ok']
      @precondition_checks.must_equal ['GET', :continued, 'GET', :continued]
    end

    it 'checks If-Match before If-Unmodified-Since' do
      res('HTTP_IF_MATCH'=>'"foo"', 'HTTP_IF_UNMODIFIED_SINCE'=>(@precondition_last_modified - 1).httpdate).must_equal [200, 'ok']
      @precondition_checks.must_equal ['GET', :continued]
    end

    it 'halts with 412 if If-Unmodified-Since is before Last-Modified and If-Match is not given' do
      res('HTTP_IF_UNMODIFIED_SINCE'=>(@precondition_last_modified - 1).httpdate).must_equal [412, '']
      @precondition_checks.must_equal ['GET']
    end

    it 'continues processing if If-Unmodified-Since is not before Last-Modified' do
      res('HTTP_IF_UNMODIFIED_SINCE'=>@precondition_last_modified.httpdate).must_equal [200, 'ok']
      res('HTTP_IF_UNMODIFIED_SINCE'=>(@precondition_last_modified + 1).httpdate).must_equal [200, 'ok']
      @precondition_checks.must_equal ['GET', :continued, 'GET', :continued]
    end

    it 'checks If-Match before If-None-Match' do
      res('HTTP_IF_MATCH'=>'"bar"', 'HTTP_IF_NONE_MATCH'=>'"foo"').must_equal [412, '']
      @precondition_checks.must_equal ['GET']
    end

    it 'checks If-None-Match before If-Modified-Since' do
      res('HTTP_IF_NONE_MATCH'=>'"foo"', 'HTTP_IF_MODIFIED_SINCE'=>(@precondition_last_modified - 1).httpdate).must_equal [304, '']
      @precondition_checks.must_equal ['GET']
    end
  end

  describe 'for PUT requests' do
    def res(env={})
      super(env.merge('REQUEST_METHOD'=>'PUT'))
    end

    it 'continues processing when no preconditions are given' do
      res.must_equal [200, 'ok']
      @precondition_checks.must_equal ['PUT', :continued]
    end

    it 'halts with 412 if If-None-Match matches' do
      res('HTTP_IF_NONE_MATCH'=>'"foo"').must_equal [412, '']
      @precondition_checks.must_equal ['PUT']
    end

    it 'continues if If-None-Match does not match' do
      res('HTTP_IF_NONE_MATCH'=>'"bar"').must_equal [200, 'ok']
      @precondition_checks.must_equal ['PUT', :continued]
    end

    it 'does not use If-Modified-Since to return a 304' do
      res('HTTP_IF_MODIFIED_SINCE'=>(@precondition_last_modified + 1).httpdate).must_equal [200, 'ok']
      @precondition_checks.must_equal ['PUT', :continued]
    end

    it 'halts with 412 if If-Unmodified-Since is before Last-Modified' do
      res('HTTP_IF_UNMODIFIED_SINCE'=>(@precondition_last_modified - 1).httpdate).must_equal [412, '']
      @precondition_checks.must_equal ['PUT']
    end

    it 'continues processing if If-Unmodified-Since is not before Last-Modified' do
      res('HTTP_IF_UNMODIFIED_SINCE'=>@precondition_last_modified.httpdate).must_equal [200, 'ok']
      @precondition_checks.must_equal ['PUT', :continued]
    end
  end

  describe 'for HEAD requests' do
    it 'halts with 304 if If-None-Match matches' do
      res('REQUEST_METHOD'=>'HEAD', 'HTTP_IF_NONE_MATCH'=>'"foo"').must_equal [304, '']
      @precondition_checks.must_equal ['HEAD']
    end

    it 'halts with 304 if If-Modified-Since is not before Last-Modified' do
      res('REQUEST_METHOD'=>'HEAD', 'HTTP_IF_MODIFIED_SINCE'=>(@precondition_last_modified + 1).httpdate).must_equal [304, '']
      @precondition_checks.must_equal ['HEAD']
    end
  end

  describe 'for other request methods' do
    it 'halts with 412 and not 304 if If-None-Match matches' do
      %w'POST DELETE PATCH OPTIONS TRACE'.each do |method|
        res('REQUEST_METHOD'=>method, 'HTTP_IF_NONE_MATCH'=>'"foo"').must_equal [412, '']
      end
      @precondition_checks.must_equal %w'POST DELETE PATCH OPTIONS TRACE'
    end
  end

  describe 'with only one validator' do
    it 'skips etag-based preconditions when only last_modified is given' do
      res('PATH_INFO'=>'/last_modified_only', 'HTTP_IF_NONE_MATCH'=>'"foo"').must_equal [200, 'ok']
      res('PATH_INFO'=>'/last_modified_only', 'HTTP_IF_MATCH'=>'"bar"').must_equal [200, 'ok']
      @precondition_checks.must_equal ['GET', :continued, 'GET', :continued]
    end

    it 'skips time-based preconditions when only etag is given' do
      res('PATH_INFO'=>'/etag_only', 'HTTP_IF_MODIFIED_SINCE'=>(@precondition_last_modified + 1).httpdate).must_equal [200, 'ok']
      res('PATH_INFO'=>'/etag_only', 'HTTP_IF_UNMODIFIED_SINCE'=>(@precondition_last_modified - 1).httpdate).must_equal [200, 'ok']
      @precondition_checks.must_equal ['GET', :continued, 'GET', :continued]
    end
  end

  describe 'with a weak etag' do
    it 'uses the weak form of the etag in the ETag header' do
      header(RodaResponseHeaders::ETAG, 'PATH_INFO'=>'/weak').must_equal 'W/"foo"'
    end

    it 'halts with 304 if If-None-Match includes the strong form of the etag' do
      res('PATH_INFO'=>'/weak', 'HTTP_IF_NONE_MATCH'=>'"foo"').must_equal [304, '']
    end

    it 'halts with 412 if If-Match is given an etag, as strong comparison never matches a weak etag' do
      res('PATH_INFO'=>'/weak', 'HTTP_IF_MATCH'=>'"foo"').must_equal [412, '']
      res('PATH_INFO'=>'/weak', 'HTTP_IF_MATCH'=>'W/"foo"').must_equal [412, '']
      @precondition_checks.must_equal ['GET', 'GET']
    end

    it 'matches If-Match * for a weak etag as long as the resource exists' do
      res('PATH_INFO'=>'/weak', 'HTTP_IF_MATCH'=>'*').must_equal [200, 'ok']
      @precondition_checks.must_equal ['GET', :continued]
    end
  end

  describe 'with an existing non-2xx status' do
    it 'still sets validators but does not halt' do
      s, h, b = req('PATH_INFO'=>'/preset_status', 'status'=>'499', 'HTTP_IF_NONE_MATCH'=>'"foo"')
      [s, b.join].must_equal [499, 'ok']
      h[RodaResponseHeaders::ETAG].must_equal '"foo"'
      h[RodaResponseHeaders::LAST_MODIFIED].must_equal @precondition_last_modified.httpdate
      @precondition_checks.must_equal ['GET', :continued]
    end

    it 'checks preconditions for a 2xx status' do
      s, h, b = req('PATH_INFO'=>'/preset_status', 'status'=>'201', 'HTTP_IF_NONE_MATCH'=>'"foo"')
      [s, b.join].must_equal [304, '']
      h[RodaResponseHeaders::ETAG].must_equal '"foo"'
      @precondition_checks.must_equal ['GET']
    end
  end

  describe 'when the same resource is requested with different methods and multiple preconditions' do
    def precondition_status(method, env)
      status(env.merge('REQUEST_METHOD'=>method, 'PATH_INFO'=>'/'))
    end

    it 'returns the same 304 status for repeated GET requests regardless of header order' do
      envs = [
        {'HTTP_IF_MATCH'=>'"foo"', 'HTTP_IF_NONE_MATCH'=>'"foo"', 'HTTP_IF_MODIFIED_SINCE'=>@precondition_last_modified.httpdate},
        {'HTTP_IF_MODIFIED_SINCE'=>@precondition_last_modified.httpdate, 'HTTP_IF_NONE_MATCH'=>'"foo"', 'HTTP_IF_MATCH'=>'"foo"'},
      ]
      envs.each do |env|
        precondition_status('GET', env).must_equal 304
      end
      @precondition_checks.must_equal ['GET', 'GET']
    end

    it 'returns the same 412 status for repeated PUT requests regardless of header order' do
      envs = [
        {'HTTP_IF_MATCH'=>'"foo"', 'HTTP_IF_NONE_MATCH'=>'"foo"', 'HTTP_IF_UNMODIFIED_SINCE'=>@precondition_last_modified.httpdate},
        {'HTTP_IF_UNMODIFIED_SINCE'=>@precondition_last_modified.httpdate, 'HTTP_IF_NONE_MATCH'=>'"foo"', 'HTTP_IF_MATCH'=>'"foo"'},
      ]
      envs.each do |env|
        precondition_status('PUT', env).must_equal 412
      end
      @precondition_checks.must_equal ['PUT', 'PUT']
    end

    it 'only continues processing when all preconditions pass' do
      precondition_status('GET', 'HTTP_IF_MATCH'=>'"foo"', 'HTTP_IF_NONE_MATCH'=>'"bar"', 'HTTP_IF_UNMODIFIED_SINCE'=>(@precondition_last_modified - 1).httpdate).must_equal 200
      @precondition_checks.must_equal ['GET', :continued]

      @precondition_checks.clear
      precondition_status('PUT', 'HTTP_IF_MATCH'=>'"bar"', 'HTTP_IF_NONE_MATCH'=>'"bar"', 'HTTP_IF_UNMODIFIED_SINCE'=>(@precondition_last_modified - 1).httpdate).must_equal 412
      @precondition_checks.must_equal ['PUT']
    end

    it 'does not perform side effects when a later precondition fails' do
      @side_effects = []
      side_effects = @side_effects
      lm = @precondition_last_modified
      app(:caching) do |r|
        r.is 'write' do
          r.preconditions(etag: 'foo', last_modified: lm)
          side_effects << :performed
          'ok'
        end
      end
      status('PATH_INFO'=>'/write', 'REQUEST_METHOD'=>'PUT', 'HTTP_IF_MATCH'=>'"foo"', 'HTTP_IF_NONE_MATCH'=>'"foo"').must_equal 412
      @side_effects.must_equal []
      status('PATH_INFO'=>'/write', 'REQUEST_METHOD'=>'PUT', 'HTTP_IF_MATCH'=>'"foo"', 'HTTP_IF_NONE_MATCH'=>'"bar"').must_equal 200
      @side_effects.must_equal [:performed]
    end
  end

  it 'warns for positional hash usage and unexpected keywords' do
    deprecated do
      app(:caching) do |r|
        r.preconditions({'etag'=>'foo', 'bad'=>true})
        'ok'
      end
      body.must_equal 'ok'
    end
  end
end


