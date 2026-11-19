require_relative "../spec_helper"

describe "module_include plugin" do 
  it "must_include given module in request or response class" do
    app(:bare) do
      plugin :module_include
      request_module(Module.new{def h; halt response.finish end})
      response_module(Module.new{def finish; [212, {}, []] end})

      route do |r|
        r.h
      end
    end

    req.must_equal [212, {}, []]
  end

  it "should accept blocks and turn them into modules" do
    app(:bare) do
      plugin :module_include
      request_module{def h; halt response.finish end}
      response_module{def finish; [212, {}, []] end}

      route do |r|
        r.h
      end
    end

    req.must_equal [212, {}, []]
  end

  it "should work if called multiple times with a block" do
    app(:bare) do
      plugin :module_include
      request_module{def h; halt response.f end}
      request_module{def i; h end}
      response_module{def f; finish end}
      response_module{def finish; [212, {}, []] end}

      route do |r|
        r.i
      end
    end

    req.must_equal [212, {}, []]
  end

  it "should not allow both blocks and modules to be passed in single call" do
    app(:bare){}
    @app.plugin :module_include
    proc{@app.request_module(Module.new){}}.must_raise Roda::RodaError
  end

  it "allows calling without block or module" do
    app(:bare){}
    @app.plugin :module_include
    @app.request_module
  end
end
