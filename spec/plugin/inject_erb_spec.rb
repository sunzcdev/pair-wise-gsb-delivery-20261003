require_relative "../spec_helper"

begin
  require 'tilt/erb'
rescue LoadError
  warn "tilt not installed, skipping inject_erb plugin test"
else
describe "inject_erb plugin" do
  before do
    app(:bare) do
      plugin :render, :views => './spec/views'
      plugin :inject_erb

      route do |r|
        r.root do
          render(:inline => "<% inject_erb('foo') %>")
        end
        r.get 'to_s' do
          render(:inline => "<% inject_erb(1) %>")
        end
        r.get 'inject' do
          render(:inline => "<% some_method do %>foo<% end %>")
        end
      end

      def some_method
        inject_erb "bar"
        yield
        inject_erb "baz"
      end
    end
  end

  it "should allow injecting into erb template" do
    body.strip.must_equal "foo"
  end

  it "should convert argument to string" do
    body('/to_s').strip.must_equal "1"
  end

  it "should work with the inject_erb plugin" do
    body('/inject').strip.must_equal "barfoobaz"
  end
end
end
