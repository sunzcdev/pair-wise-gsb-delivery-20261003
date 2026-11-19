require_relative "../spec_helper"

begin
  require 'tilt/erb'
rescue LoadError
  warn "tilt not installed, skipping partials plugin test"  
else
  describe "partials plugin" do
    before do
      app(:bare) do
        plugin :partials, :views=>"./spec/views"

        route do |r|
          r.is "partial" do
            partial("test", :locals=>{:title => "About Roda"})
          end

          r.is "partial/subdir" do
            partial("about/test", :locals=>{:title => "About Roda"})
          end

          r.is "partial/inline" do
            partial(:inline=>"Hello <%= name %>", :locals=>{:name => "Agent Smith"})
          end
        
        end
      end
    end

    it "partial renders without layout, and prepends _ to template" do
      body("/partial").strip.must_equal "<h1>About Roda</h1>"
    end

    it "partial renders without layout, and prepends _ to template" do
      body("/partial/subdir").strip.must_equal "<h1>Subdir: About Roda</h1>"
    end

    it "partial handles inline partials" do
      body("/partial/inline").strip.must_equal "Hello Agent Smith"
    end
  end
end

begin
  require 'tilt'
  require 'tilt/string'
  require 'tilt/rdoc'
  require_relative '../../lib/roda/plugins/render'
rescue LoadError
  warn "tilt not installed, skipping each_partial test"  
else
describe "each_partial method in partials plugin" do 
  [true, false].each do |cache|
    it "calls render with each argument, returning joined string with all results in cache: #{cache} mode" do
      app(:bare) do
        plugin :render, :views=>'spec/views', :engine=>'str', :cache=>cache
        plugin :partials

        o = Object.new
        def o.to_s; 'each' end

        route do |r|
          r.root do
            each_partial([1,2,3], :each)
          end

          r.is 'a' do
            each_partial([1,2,3], :each, :local=>:foo, :bar=>4)
          end

          r.is 'b' do
            each_partial([1,2,3], :each, :local=>nil)
          end

          r.is 'c' do
            each_partial([1,2,3], :each, :locals=>{:foo=>4})
          end

          r.is 'e' do
            each_partial([1,2,3], o)
          end

          r.is 'f' do
            each_partial([1,2,3], "views/each", :views=>'spec')
          end

          r.is 'g' do
            each_partial([1,2,3], "each.foo")
          end
        end
      end

      3.times do
        body.must_equal "x-1-\nx-2-\nx-3-\n"
        body("/a").must_equal "x--1\nx--2\nx--3\n"
        body("/b").must_equal "x--\nx--\nx--\n"
        body("/c").must_equal "x-1-4\nx-2-4\nx-3-4\n"
        body("/e").must_equal "x-1-\nx-2-\nx-3-\n"
        body("/f").must_equal "x-1-\nx-2-\nx-3-\n"
        body("/g").must_equal "y-1-\ny-2-\ny-3-\n"
      end
    end

    it "bases local name on basename of template in cache: #{cache} mode" do
      app(:bare) do
        plugin :render, :views=>'spec', :engine=>'str', :cache=>cache
        plugin :partials

        route do |r|
          r.root do
            each_partial([1,2,3], "views/each")
          end
        end
      end

      3.times do
        body.must_equal "x-1-\nx-2-\nx-3-\n"
      end
    end
  end
end
end
