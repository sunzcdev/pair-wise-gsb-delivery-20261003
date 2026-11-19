require_relative "../spec_helper"

describe "redirect_path plugin" do 
  before do
    app(:bare) do 
      plugin :redirect_path
      foo = Struct.new(:id).new(1)
      path foo.class do |foo|
        "/foo/#{foo.id}"
      end
      route do|r|
        r.post("none"){r.redirect}
        r.get("string"){r.redirect "/string"}
        r.get("foo"){r.redirect foo}
        r.get("suffix"){r.redirect foo, "/status"}
      end
    end
  end

  it "allows normal use of redirect if given no arguments" do
    s, h = req("/none", "REQUEST_METHOD"=>"POST")
    s.must_equal 302
    h[RodaResponseHeaders::LOCATION].must_equal "/none"
  end

  it "allows normal use of redirect if given a string argument" do
    s, h = req("/string")
    s.must_equal 302
    h[RodaResponseHeaders::LOCATION].must_equal "/string"
  end

  it "uses path method to determine path if given a non-string argument" do
    s, h = req("/foo")
    s.must_equal 302
    h[RodaResponseHeaders::LOCATION].must_equal "/foo/1"
  end

  it "supports suffix to path as a second argument if given a non-string first argument" do
    s, h = req("/suffix")
    s.must_equal 302
    h[RodaResponseHeaders::LOCATION].must_equal "/foo/1/status"
  end
end
