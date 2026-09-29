require_relative "../spec_helper"

describe "tentative plugin" do
  it "commits changes when the branch handles the request" do
    app(:tentative) do |r|
      r.tentative do
        r.on "foo", Integer do |id|
          response['X-Branch'] = 'first'
          r.abandon if id > 10
          "first #{id}"
        end
        r.abandon
      end

      r.on "foo", Integer do |id|
        "second #{id}"
      end
    end

    body('/foo/1').must_equal 'first 1'
    header('X-Branch', '/foo/1').must_equal 'first'
  end

  it "restores path, captures, and response state when the branch is abandoned" do
    app(:tentative) do |r|
      r.tentative do
        r.on "foo", Integer do |id|
          response['X-Branch'] = 'first'
          response.status = 201
          response.write('first')
          r.abandon
        end
        r.abandon
      end

      r.on "foo", Integer do |id|
        "second #{id}"
      end
    end

    body('/foo/1').must_equal 'second 1'
    header('X-Branch', '/foo/1').must_be_nil
    status('/foo/1').must_equal 200
  end

  it "restores remaining path and captures before continuing routing" do
    seen = []
    app(:tentative) do |r|
      r.tentative do
        r.on "foo", Integer do |id|
          r.abandon
        end
        r.abandon
      end

      seen << r.remaining_path.dup
      seen << r.captures.dup
      "main"
    end

    body('/foo/1').must_equal 'main'
    seen.must_equal ['/foo/1', []]
  end

  it "commits the block return value if the branch neither halts nor abandons" do
    app(:tentative) do |r|
      r.tentative do
        response['X-Branch'] = 'first'
        "tentative"
      end

      "main"
    end

    body.must_equal 'tentative'
    header('X-Branch').must_equal 'first'
  end

  it "restores only the inner state when nested tentative branches are abandoned" do
    seen = []
    app(:tentative) do |r|
      r.tentative do
        response['X-Outer'] = '1'
        r.tentative do
          response['X-Inner'] = '1'
          r.abandon
        end
        seen << [response['X-Outer'], response['X-Inner']]
        r.abandon
      end

      "main"
    end

    body.must_equal 'main'
    seen.must_equal [['1', nil]]
    header('X-Outer').must_be_nil
    header('X-Inner').must_be_nil
  end

  it "commits enclosing branch changes when a nested tentative branch abandons and the enclosing branch handles the request" do
    app(:tentative) do |r|
      r.tentative do
        response['X-Outer'] = '1'
        r.tentative do
          response['X-Inner'] = '1'
          r.abandon
        end
        "outer"
      end

      "main"
    end

    body.must_equal 'outer'
    header('X-Outer').must_equal '1'
    header('X-Inner').must_be_nil
  end

  it "restores a rewindable request body when the branch is abandoned" do
    app(:tentative) do |r|
      r.tentative do
        r.post "foo" do
          r.body.read
          r.abandon
        end
        r.abandon
      end

      r.post "foo" do
        "body: #{r.body.read}"
      end
    end

    if ENV['LINT']
      # Rack::Lint's input wrapper does not support rewind, so the
      # request body cannot be restored in this case.
      proc{req('/foo', 'REQUEST_METHOD'=>'POST', 'rack.input'=>rack_input('abc'))}.must_raise(Roda::RodaError)
    else
      body('/foo', 'REQUEST_METHOD'=>'POST', 'rack.input'=>rack_input('abc')).must_equal 'body: abc'
    end
  end

  it "raises instead of restoring when the branch read a request body that cannot be rewound" do
    app(:tentative) do |r|
      r.tentative do
        r.post "foo" do
          r.body.read
          r.abandon
        end
        r.abandon
      end

      r.post "foo" do
        "main"
      end
    end

    input = rack_input('abc')
    def input.rewind(*) raise IOError, 'non-rewindable' end
    proc{req('/foo', 'REQUEST_METHOD'=>'POST', 'rack.input'=>input)}.must_raise(Roda::RodaError)
  end

  it "raises instead of restoring when the branch parsed form data from a request body that cannot be rewound" do
    app(:tentative) do |r|
      r.tentative do
        r.post "foo" do
          r.params
          r.abandon
        end
        r.abandon
      end

      r.post "foo" do
        "main"
      end
    end

    input = rack_input('a=1')
    def input.rewind(*) raise IOError, 'non-rewindable' end
    env = {'REQUEST_METHOD'=>'POST', 'rack.input'=>input, 'CONTENT_TYPE'=>'application/x-www-form-urlencoded', 'CONTENT_LENGTH'=>'3'}
    proc{req('/foo', env)}.must_raise(Roda::RodaError)
  end
end
