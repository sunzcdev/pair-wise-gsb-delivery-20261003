require_relative "spec_helper"

describe "r.run" do
  it "should allow composition of apps" do
    a = app do |r|
      r.on "services", :id do |id|
        "View #{id}"
      end
    end

    app(:new) do |r|
      r.on "provider" do
        r.run a
      end
    end

    body("/provider/services/101").must_equal 'View 101'
  end

  it "modifies SCRIPT_NAME/PATH_INFO when calling run" do
    a = app{|r| "#{r.script_name}|#{r.path_info}"}
    app{|r| r.on("a"){r.run a}}
    body("/a/b").must_equal "/a|/b"
  end

  it "restores SCRIPT_NAME/PATH_INFO before returning from run" do
    a = app{|r| "#{r.script_name}|#{r.path_info}"}
    x = nil
    app do |r|
      s = catch(:halt){r.on("a"){r.run a}}
      x = s[2]
      x.close if x.respond_to?(:close)
      "#{r.script_name}|#{r.path_info}"
    end
    body("/a/b").must_equal "|/a/b"
    x = '/a|/b'
  end
end
