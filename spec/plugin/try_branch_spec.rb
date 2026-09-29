require_relative "../spec_helper"

describe "try_branch plugin" do
  it "rolls back headers, captures, status, and path when the branch abandons" do
    app(:try_branch) do |r|
      r.try_branch "foo", String do |seg|
        response['X-First'] = seg
        response.status = 201
        r.abandon_branch
      end

      r.on "foo", String do |seg|
        response['X-Second'] = seg
        "second #{seg}"
      end
    end

    body('/foo/bar').must_equal 'second bar'
    header('X-Second', '/foo/bar').must_equal 'bar'
    header('X-First', '/foo/bar').must_be_nil
    status('/foo/bar').must_equal 200
  end

  it "rolls back the response body when the branch abandons" do
    app(:try_branch) do |r|
      r.try_branch "foo" do
        response.write "tentative"
        r.abandon_branch
      end

      r.on "foo" do
        "final"
      end
    end

    body('/foo').must_equal 'final'
    header('Content-Length', '/foo').must_equal '5'
  end

  it "rolls back when the branch returns without handling the request" do
    app(:try_branch) do |r|
      r.try_branch "foo" do
        response['X-First'] = '1'
        "ignored"
      end

      r.on "foo" do
        "final"
      end
    end

    body('/foo').must_equal 'final'
    header('X-First', '/foo').must_be_nil
  end

  it "commits changes when the branch handles the request" do
    app(:try_branch) do |r|
      r.try_branch "foo", String do |seg|
        response['X-First'] = seg
        r.is do
          "first #{seg}"
        end
      end

      r.on "foo", String do |seg|
        "second #{seg}"
      end
    end

    body('/foo/bar').must_equal 'first bar'
    header('X-First', '/foo/bar').must_equal 'bar'
  end

  it "does not call the block when the matchers do not match" do
    called = false
    app(:try_branch) do |r|
      r.try_branch "foo" do
        called = true
      end

      r.is "bar" do
        "bar"
      end
    end

    body('/bar').must_equal 'bar'
    called.must_equal false
  end

  it "supports try_branch without matchers" do
    app(:try_branch) do |r|
      r.try_branch do
        response['X-First'] = '1'
        r.abandon_branch
      end

      r.root do
        "root"
      end
    end

    body.must_equal 'root'
    header('X-First').must_be_nil
  end

  it "restores state at inner branch entry when nested branches abandon" do
    app(:try_branch) do |r|
      r.try_branch "a" do
        response['X-Outer'] = 'outer'

        r.try_branch "b" do
          response['X-Inner'] = 'inner'
          r.abandon_branch
        end

        r.is "b" do
          "outer committed"
        end
      end
    end

    body('/a/b').must_equal 'outer committed'
    header('X-Outer', '/a/b').must_equal 'outer'
    header('X-Inner', '/a/b').must_be_nil
  end

  it "restores state at outer branch entry when both nested branches abandon" do
    app(:try_branch) do |r|
      r.try_branch "a" do
        response['X-Outer'] = 'outer'

        r.try_branch "b" do
          response['X-Inner'] = 'inner'
          r.abandon_branch
        end

        r.abandon_branch
      end

      r.on "a" do
        r.is "b" do
          "fallback"
        end
      end
    end

    body('/a/b').must_equal 'fallback'
    header('X-Outer', '/a/b').must_be_nil
    header('X-Inner', '/a/b').must_be_nil
  end

  it "replays a rewindable request body read by an abandoned branch" do
    unless_lint do
      app(:try_branch) do |r|
        r.try_branch "foo" do
          r.body.read
          r.abandon_branch
        end

        r.post "foo" do
          "body: #{r.body.read}"
        end
      end

      body('/foo', 'REQUEST_METHOD'=>'POST', 'rack.input'=>rack_input('a=1')).must_equal 'body: a=1'
    end
  end

  it "restores cached form data parsed by an abandoned branch" do
    unless_lint do
      app(:try_branch) do |r|
        r.try_branch "foo" do
          r.POST
          r.abandon_branch
        end

        r.post "foo" do
          "a: #{r.POST['a']}"
        end
      end

      body('/foo', 'REQUEST_METHOD'=>'POST', 'CONTENT_TYPE'=>'application/x-www-form-urlencoded', 'rack.input'=>rack_input('a=1')).must_equal 'a: 1'
    end
  end

  it "raises instead of pretending to roll back when the branch read an unreplayable request body" do
    input = Object.new
    def input.read(*) "a=1" end
    def input.gets(*) nil end
    def input.each(*) end
    def input.rewind(*) raise IOError, "stream is not rewindable" end

    app(:try_branch) do |r|
      r.try_branch "foo" do
        r.body.read
        r.abandon_branch
      end

      r.post "foo" do
        "unreachable"
      end
    end

    proc{body('/foo', 'REQUEST_METHOD'=>'POST', 'rack.input'=>input)}.must_raise(Roda::RodaError)
  end

  it "restores the unwrapped request body in the environment" do
    before = nil
    input = nil
    app(:try_branch) do |r|
      before = r.env['rack.input']

      r.try_branch "foo" do
        r.env['rack.input'].must_be_instance_of Roda::RodaPlugins::TryBranch::InputWrapper
        r.abandon_branch
      end

      r.on "foo" do
        input = r.env['rack.input']
        "done"
      end
    end

    body('/foo', 'rack.input'=>rack_input('')).must_equal 'done'
    input.must_be_same_as before
  end
end
