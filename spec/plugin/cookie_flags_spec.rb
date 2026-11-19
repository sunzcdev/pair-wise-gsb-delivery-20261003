require_relative "../spec_helper"

describe "cookie_flags plugin" do 
  exception_class = Class.new(StandardError)

  before do
    app(:bare) do 
      plugin :cookies
      plugin :cookie_flags
      route do |r|
        r.get String, String, String do |secure, httponly, samesite|
          h = {:value=>'b', :secure=>secure == 'secure', :httponly=>httponly == 'httponly'}
          h[:same_site] = samesite.to_sym unless samesite == 'none'
          response.set_cookie('a', h)
          body = response.headers[RodaResponseHeaders::SET_COOKIE]
          response.set_cookie('b', h) if r.GET['2']
          body
        end

        r.get 'raise' do
          raise exception_class
        end

        ''
      end
    end
  end

  it "does not modify flags if they are set correctly" do
    _, h, b = req('/secure/httponly/strict')
    h = h[RodaResponseHeaders::SET_COOKIE]
    h = h[0] if h.is_a?(Array)
    b = b.join
    h.must_match(/secure/i)
    h.must_match(/httponly/i)
    h.must_match(/samesite=strict/i)
    h.must_equal b if Rack.release >= '1.6.5'
  end

  it "does not modify flags if they are set correctly when using multiple cookies" do
    _, h, b = req('/secure/httponly/strict', 'QUERY_STRING'=>'2=2')
    h = h[RodaResponseHeaders::SET_COOKIE]
    h = h[0] if h.is_a?(Array)
    b = b.join
    h.must_match(/secure/i)
    h.must_match(/httponly/i)
    h.must_match(/samesite=strict/i)
  end

  it "modifies flags if they are set correctly" do
    _, h, b = req('/nosecure/nohttponly/lax')
    h = h[RodaResponseHeaders::SET_COOKIE]
    h = h[0] if h.is_a?(Array)
    h.must_match(/secure/i)
    h.must_match(/httponly/i)
    h.must_match(/samesite=strict/i)
    b = b.join
    b.wont_match(/secure/i)
    b.wont_match(/httponly/i)
    b.wont_match(/samesite=strict/i)
    b.must_match(/samesite=lax/i) if Rack.release >= '1.6.5'
  end

  it "modifies flags if they are set correctly when using multiple cookies" do
    _, h, b = req('/nosecure/nohttponly/lax')
    h[RodaResponseHeaders::SET_COOKIE].each do |h|
      h.must_match(/secure/i)
      h.must_match(/httponly/i)
      h.must_match(/samesite=strict/i)
    end
    b = b.join
    b.wont_match(/secure/i)
    b.wont_match(/httponly/i)
    b.wont_match(/samesite=strict/i)
    b.must_match(/samesite=lax/i)
  end if Rack.release >= '3'

  it "adds samesite entry if configured and not present" do
    _, h, b = req('/nosecure/nohttponly/none')
    h = h[RodaResponseHeaders::SET_COOKIE]
    h = h[0] if h.is_a?(Array)
    h.must_match(/secure/i)
    h.must_match(/httponly/i)
    h.must_match(/samesite=strict/i)
    b = b.join
    b.wont_match(/secure/i)
    b.wont_match(/httponly/i)
    b.wont_match(/samesite/i)
  end

  it "supports checking only secure flag" do
    @app.plugin :cookie_flags, :httponly=>false, :same_site=>nil
    _, h, b = req('/nosecure/nohttponly/none')
    h = h[RodaResponseHeaders::SET_COOKIE]
    h = h[0] if h.is_a?(Array)
    h.must_match(/secure/i)
    h.wont_match(/httponly/i)
    h.wont_match(/samesite=strict/i)
    b = b.join
    b.wont_match(/secure/i)
    b.wont_match(/httponly/i)
    b.wont_match(/samesite/i)
  end

  it "supports checking only httponly flag" do
    @app.plugin :cookie_flags, :secure=>false, :same_site=>nil
    _, h, b = req('/nosecure/nohttponly/none')
    h = h[RodaResponseHeaders::SET_COOKIE]
    h = h[0] if h.is_a?(Array)
    h.wont_match(/secure/i)
    h.must_match(/httponly/i)
    h.wont_match(/samesite=strict/i)
    b = b.join
    b.wont_match(/secure/i)
    b.wont_match(/httponly/i)
    b.wont_match(/samesite/i)
  end

  it "supports checking only samesite flag" do
    @app.plugin :cookie_flags, :httponly=>false, :secure=>nil
    _, h, b = req('/nosecure/nohttponly/none')
    h = h[RodaResponseHeaders::SET_COOKIE]
    h = h[0] if h.is_a?(Array)
    h.wont_match(/secure/i)
    h.wont_match(/httponly/i)
    h.must_match(/samesite=strict/i)
    b = b.join
    b.wont_match(/secure/i)
    b.wont_match(/httponly/i)
    b.wont_match(/samesite/i)
  end

  it "supports enforcing samesite=lax" do
    @app.plugin :cookie_flags, :httponly=>false, :secure=>nil, :same_site=>:lax
    _, h, b = req('/nosecure/nohttponly/none')
    h = h[RodaResponseHeaders::SET_COOKIE]
    h = h[0] if h.is_a?(Array)
    h.wont_match(/secure/i)
    h.wont_match(/httponly/i)
    h.must_match(/samesite=lax/i)
    b = b.join
    b.wont_match(/secure/i)
    b.wont_match(/httponly/i)
    b.wont_match(/samesite/i)
  end

  it "supports enforcing samesite=none, which also turns on secure" do
    @app.plugin :cookie_flags, :httponly=>false, :secure=>nil, :same_site=>:none
    _, h, b = req('/nosecure/nohttponly/none')
    h = h[RodaResponseHeaders::SET_COOKIE]
    h = h[0] if h.is_a?(Array)
    h.must_match(/secure/i)
    h.wont_match(/httponly/i)
    h.must_match(/samesite=none/i)
    b = b.join
    b.wont_match(/secure/i)
    b.wont_match(/httponly/i)
    b.wont_match(/samesite/i)
  end

  it "supports :warn_and_modify action" do
    s = nil
    @app.plugin :cookie_flags, :action=>:warn_and_modify
    @app.send(:define_method, :warn){|msg| s = msg}
    _, h, b = req('/nosecure/nohttponly/strict')
    s.must_match(/Response contains cookie with unexpected flags:.*Expecting the following cookie flags: secure httponly/)
    h = h[RodaResponseHeaders::SET_COOKIE]
    h = h[0] if h.is_a?(Array)
    h.must_match(/secure/i)
    h.must_match(/httponly/i)
    h.must_match(/samesite=strict/i) if Rack.release >= '1.6.5'
    b = b.join
    b.wont_match(/secure/i)
    b.wont_match(/httponly/i)
    b.must_match(/samesite=strict/i) if Rack.release >= '1.6.5'
  end

  it "supports :warn action" do
    s = nil
    @app.plugin :cookie_flags, :action=>:warn
    @app.send(:define_method, :warn){|msg| s = msg}
    _, h, b = req('/secure/httponly/lax')
    s.must_match(/Response contains cookie with unexpected flags:.*Expecting the following cookie flags: samesite=strict/)
    h = h[RodaResponseHeaders::SET_COOKIE]
    h = h[0] if h.is_a?(Array)
    h.must_match(/secure/i)
    h.must_match(/httponly/i)
    h.wont_match(/samesite=strict/i)
    h.must_equal b.join
  end

  it "supports :error action" do
    @app.plugin :cookie_flags, :action=>:raise
    e = proc{req('/secure/httponly/none')}.must_raise(Roda::RodaPlugins::CookieFlags::Error)
    e.message.must_match(/Response contains cookie with unexpected flags:.*Expecting the following cookie flags: samesite=strict/)
  end

  it "should not break when exceptions are raised by app" do
    proc{req('/raise')}.must_raise(exception_class)
  end

  it "should handle response without cookies set" do
    s, h, b = req
    s.must_equal 200
    h[RodaResponseHeaders::SET_COOKIE].must_be_nil
    b.must_equal ['']
  end
end
