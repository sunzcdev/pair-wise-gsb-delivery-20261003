require_relative "../spec_helper"

describe "r plugin" do 
  it "adds r method for request access" do
    app(:r) do |_|
      r.get "foo" do
        "foo"
      end

      "root"
    end

    body.must_equal 'root'
    body("/foo").must_equal 'foo'
  end
end
