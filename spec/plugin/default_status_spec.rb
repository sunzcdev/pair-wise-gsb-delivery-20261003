require_relative "../spec_helper"

describe "default_status plugin" do
  it "sets the default response status to use for the response" do
    app(:bare) do
      plugin :default_status do
        201
      end
      route do |r|
        r.halt response.finish_with_body([])
      end
    end

    status.must_equal 201
  end

  it "should exec the plugin block in the context of the instance" do
    app(:bare) do
      plugin :default_status do
        200 + @body[0].length
      end
      route do |r|
        r.path_info
      end
    end

    status.must_equal 201
    status('/foo/bar').must_equal 208
  end

  it "should not override existing response" do
    app(:bare) do
      plugin :default_status do
        201
      end

      route do |r|
        response.status = 202
        r.halt response.finish_with_body([])
      end
    end

    status.must_equal 202
  end

  it "should work correctly in subclasses" do
    app(:bare) do
      plugin :default_status do
        201
      end

      route do |r|
        r.halt response.finish_with_body([])
      end
    end

    @app = Class.new(@app)

    status.must_equal 201
  end

  it "should raise if not given a block" do
    proc{app(:default_status)}.must_raise Roda::RodaError
  end

  [true, false].each do |warn_arity|
    send(warn_arity ? :deprecated : :it, "works with blocks with invalid arity") do
      app(:bare) do
        opts[:check_arity]  = :warn if warn_arity
        plugin :default_status do |r|
          201
        end
        route do |r|
          r.halt response.finish_with_body([])
        end
      end

      status.must_equal 201
    end
  end

  it "does not work with blocks with invalid arity if :check_arity app option is false" do
    app(:bare) do
      opts[:check_arity] = false
      plugin :default_status do |r|
        201
      end
      route do |r|
        r.halt response.finish_with_body([])
      end
    end

    proc{status}.must_raise ArgumentError
  end
end
