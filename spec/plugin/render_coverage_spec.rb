require_relative "../spec_helper"

begin
  require 'tilt'
  require 'tilt/erb'
  raise LoadError unless Tilt::Template.method_defined?(:compiled_path=)
rescue LoadError
  warn "tilt 2.1+ not installed, skipping render_coverage plugin test"  
else

require 'fileutils'

describe "render_coverage plugin" do
  coverage_dir = "./spec/render_coverage-#{$$}"

  define_method(:setup_app) do |render_opts={}, render_coverage_opts={}, prefix=''|
    app(:bare) do
      if render_opts == :scope_class_and_fixed_locals
        render_opts = {:template_opts=>{:scope_class=>self, :default_fixed_locals=>'()'}}
      end

      plugin :render, {:views=>"./spec/views/about", :check_paths=>true, :layout=>false}.merge!(render_opts)
      plugin :render_coverage, {:dir=>coverage_dir}.merge!(render_coverage_opts)
      plugin :render_coverage

      route do |r|
        r.get "inline" do
          render(:inline=>"il")
        end

        r.get "path" do
          render(:path=>"./spec/views/about.erb", :locals=>{:title => "About Roda"}, :template_opts=>{:fixed_locals=>'(title: raise)'})
        end

        r.get "not-exist" do
          template_path(find_template(parse_template_opts("#{prefix}not-exist", {})))
        end

        r.get "view" do
          view("#{prefix}comp_test")
        end

        r.get "nested-rel" do
          render("#{prefix}nested/../nested/comp_test")
        end

        r.get "nested" do
          render("#{prefix}nested/comp_test")
        end

        r.get "root" do
          render("#{prefix}comp_test")
        end
      end
    end
    Object.const_set(:RodaRenderCoverage, @app)
  end
  after do
    Object.send(:remove_const, :RodaRenderCoverage)
    FileUtils.rm_r(coverage_dir)
  end

  {
    [] => "should store files in specified directory",
    [:scope_class_and_fixed_locals] => "should handle using of :scope_class and fixed_locals",
    [{:cache=>false}] => "should handle cache: false render plugin option",
    [{:views=>'./spec/views', :allowed_paths=>%w'./spec/views/about'}, {}, 'about/'] => "should strip paths based on render plugin :allowed_paths option",
    [{:views=>'./spec/views'}, {:strip_paths=>%w'./spec/views/about'}, 'about/'] => "should strip paths based on render_coverage plugin :strip_paths option"

  }.each do |args, desc|
    it desc do 
      setup_app(*args)
      Dir["#{coverage_dir}/*"].sort.must_equal %w''
      body("/root").strip.must_equal "about-ct"
      Dir["#{coverage_dir}/*"].map{|f| File.basename(f)}.sort.must_equal %w'comp_test.erb.rb'
      body("/nested").strip.must_equal "about-nested-ct"
      Dir["#{coverage_dir}/*"].map{|f| File.basename(f)}.sort.must_equal %w'comp_test.erb.rb nested-comp_test.erb.rb'
      body("/path").strip.must_equal "<h1>About Roda</h1>"
      Dir["#{coverage_dir}/*"].map{|f| File.basename(f)}.sort.must_equal %w'comp_test.erb.rb nested-comp_test.erb.rb'
      body("/inline").strip.must_equal "il"
      Dir["#{coverage_dir}/*"].map{|f| File.basename(f)}.sort.must_equal %w'comp_test.erb.rb nested-comp_test.erb.rb'
      Dir["#{coverage_dir}/*"].map{|f| File.delete(f)}
      body("/view").strip.must_equal "about-ct"
      Dir["#{coverage_dir}/*"].map{|f| File.basename(f)}.sort.must_equal %w''
      body("/nested-rel").strip.must_equal "about-nested-ct"
      Dir["#{coverage_dir}/*"].map{|f| File.basename(f)}.sort.must_equal %w''
      body("/not-exist").must_include 'spec/views/about/not-exist.erb'
      Dir["#{coverage_dir}/*"].map{|f| File.basename(f)}.sort.must_equal %w''
    end
  end
end
end
