require_relative "../spec_helper"

begin
  require 'tilt'
  require 'tilt/erb'
  require 'tilt/string'
  require_relative '../../lib/roda/plugins/render'
rescue LoadError
  warn "tilt not installed, skipping render plugin test"  
else
describe "additional_render_engines plugin" do 
  it "supports additional render engines" do
    app(:bare) do
      plugin :render, :views=>'spec/views'
      plugin :additional_render_engines, ['str', 'html']
      route do |r|
        render(r.remaining_path[1, 1000]).strip
      end
    end

    body('/a').must_equal 'a'
    body('/a1').must_equal 'a1-str'
    body('/a2').must_equal 'a2-html'
    body('/a3').must_equal 'a3-erb'

    proc{body('/nonexistent')}.must_raise Errno::ENOENT
  end
end
end
