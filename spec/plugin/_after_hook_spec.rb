require_relative "../spec_helper"

describe "deprecated _after_hook plugin" do 
  it "shouldn't break things" do
    x = []
    app(:_after_hook) do |r|
      x << 0
      'a'
    end
    @app.send(:include, Module.new do
      define_method(:_roda_after_00_test){|_| x << 1}
      private :_roda_after_00_test
    end)

    body.must_equal 'a'
    x.must_equal [0, 1]
  end
end

