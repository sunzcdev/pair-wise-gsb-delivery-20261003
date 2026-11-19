require_relative "../spec_helper"

describe "assume_ssl plugin" do 
  it "makes r.ssl? always return true" do
    app(:assume_ssl) do |r|
      r.ssl?.to_s
    end

    body.must_equal 'true'
  end
end
