require_relative "../spec_helper"

describe "host_authorization plugin" do 
  it "allows configuring authorized hosts" do
    app do |r|
      r.get 'x' do
        '2'
      end
      check_host_authorization!
      '1'
    end
    app.plugin :host_authorization, 'foo.example.com'

    status.must_equal 403
    body.must_equal ''
    status('/x').must_equal 200
    body('/x').must_equal '2'
    status('HTTP_HOST'=>'foo.example.com').must_equal 200
    status('HTTP_HOST'=>'bar.example.com').must_equal 403
    status('HTTP_HOST'=>'foo.example.com:80').must_equal 200
    status('HTTP_HOST'=>'bar.example.com:80').must_equal 403

    app.plugin :host_authorization, /\A(foo|bar)\.example\.com\z/
    status.must_equal 403
    body.must_equal ''
    status('HTTP_HOST'=>'foo.example.com').must_equal 200
    status('HTTP_HOST'=>'bar.example.com').must_equal 200
    status('HTTP_HOST'=>'baz.example.com').must_equal 403

    app.plugin :host_authorization, %w'foo.example.com bar.example.com'
    status.must_equal 403
    body.must_equal ''
    status('HTTP_HOST'=>'foo.example.com').must_equal 200
    status('HTTP_HOST'=>'bar.example.com').must_equal 200
    status('HTTP_HOST'=>'baz.example.com').must_equal 403

    app.plugin :host_authorization, %w'foo.example.com bar.example.com'
    status.must_equal 403
    body.must_equal ''
    status('HTTP_HOST'=>'foo.example.com').must_equal 200
    status('HTTP_HOST'=>'bar.example.com').must_equal 200
    status('HTTP_HOST'=>'baz.example.com').must_equal 403

    status('HTTP_HOST'=>'foo.example.com', 'HTTP_X_FORWARDED_HOST'=>'x.example.com').must_equal 200
    status('HTTP_HOST'=>'bar.example.com', 'HTTP_X_FORWARDED_HOST'=>'x.example.com').must_equal 200
    status('HTTP_HOST'=>'baz.example.com', 'HTTP_X_FORWARDED_HOST'=>'x.example.com').must_equal 403

    status('HTTP_HOST'=>'baz.example.com', 'HTTP_X_FORWARDED_HOST'=>'bar.example.com').must_equal 403
    status('HTTP_HOST'=>'baz.example.com', 'HTTP_X_FORWARDED_HOST'=>'x.example.com, bar.example.com').must_equal 403
    status('HTTP_HOST'=>'baz.example.com', 'HTTP_X_FORWARDED_HOST'=>'bar.example.com, x.example.com').must_equal 403
    status('HTTP_HOST'=>'baz.example.com', 'HTTP_X_FORWARDED_HOST'=>'x.example.com, bar.example.com:80').must_equal 403

    app.plugin :host_authorization, %w'foo.example.com bar.example.com', :check_forwarded=>true
    status('HTTP_HOST'=>'foo.example.com').must_equal 200
    status('HTTP_HOST'=>'bar.example.com').must_equal 200
    status('HTTP_HOST'=>'baz.example.com').must_equal 403

    status('HTTP_HOST'=>'foo.example.com', 'HTTP_X_FORWARDED_HOST'=>'x.example.com').must_equal 200
    status('HTTP_HOST'=>'bar.example.com', 'HTTP_X_FORWARDED_HOST'=>'x.example.com').must_equal 200
    status('HTTP_HOST'=>'baz.example.com', 'HTTP_X_FORWARDED_HOST'=>'x.example.com').must_equal 403

    status('HTTP_HOST'=>'baz.example.com', 'HTTP_X_FORWARDED_HOST'=>'bar.example.com').must_equal 200
    status('HTTP_HOST'=>'baz.example.com', 'HTTP_X_FORWARDED_HOST'=>'x.example.com, bar.example.com').must_equal 200
    status('HTTP_HOST'=>'baz.example.com', 'HTTP_X_FORWARDED_HOST'=>'bar.example.com, x.example.com').must_equal 403
    status('HTTP_HOST'=>'baz.example.com', 'HTTP_X_FORWARDED_HOST'=>'x.example.com, bar.example.com:80').must_equal 200

    app.plugin :host_authorization, 'foo.example.com' do |r|
      response.status = 401
      '2'
    end

    status.must_equal 401
    body.must_equal '2'
    status('HTTP_HOST'=>'foo.example.com').must_equal 200
    status('HTTP_HOST'=>'bar.example.com').must_equal 401
    status('HTTP_HOST'=>'foo.example.com:80').must_equal 200
    status('HTTP_HOST'=>'bar.example.com:80').must_equal 401
  end
end
