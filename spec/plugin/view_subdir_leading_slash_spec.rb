require_relative "../spec_helper"

begin
  require 'tilt/erb'
rescue LoadError
  warn "tilt not installed, skipping view_subdir_leading_slash plugin test"  
else
describe "view_options plugin view subdirs" do
  before do
    app(:bare) do
      plugin :render, :views=>"spec"
      plugin :view_subdir_leading_slash

      route do |r|
        set_view_subdir "views"
        r.get("a"){render("comp_test")}
        r.get("b"){render("./comp_test")}
        render("/views/comp_test")
      end
    end
  end

  it "should use view subdir if template does not contain /" do
    body("/a").strip.must_equal "ct"
  end

  it "should use view subdir if template contains slash but does not start with /" do
    body("/b").strip.must_equal "ct"
  end

  it "should not use view subdir if template starts with /" do
    body.strip.must_equal "ct"
  end
end
end
