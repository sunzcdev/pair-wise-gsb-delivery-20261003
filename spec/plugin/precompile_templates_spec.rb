require_relative "../spec_helper"

begin
  require 'tilt/erb'
rescue LoadError
  warn "tilt not installed, skipping precompiled_templates plugin test"  
else
describe "precompile_templates plugin - precompile_templates method" do 
  it "adds support for template precompilation" do
    app(:bare) do
      plugin :render, :views=>'spec/views'
      plugin :precompile_templates
      route do |r|
        @a = 1
        render('iv')
      end
    end

    app.render_opts[:cache][File.expand_path('spec/views/iv.erb')].must_be_nil
    app.precompile_templates 'spec/views/iv.erb'
    app.render_opts[:cache][File.expand_path('spec/views/iv.erb')].wont_equal nil
    app.render_opts[:cache][File.expand_path('spec/views/iv.erb')].instance_variable_get(:@compiled_method).length.must_equal 1
    body.strip.must_equal '1'
    app.render_opts[:cache][File.expand_path('spec/views/iv.erb')].instance_variable_get(:@compiled_method).length.must_equal 1
  end

  it "adds support for template precompilation with :locals" do
    app(:bare) do
      plugin :render, :views=>'spec/views'
      plugin :precompile_templates
      route do |r|
        render('about', :locals=>{:title=>'1'})
      end
    end

    app.render_opts[:cache][File.expand_path('spec/views/about.erb')].must_be_nil
    app.precompile_templates 'spec/views/about.erb', :locals=>[:title]
    app.render_opts[:cache][File.expand_path('spec/views/about.erb')].wont_equal nil
    app.render_opts[:cache][File.expand_path('spec/views/about.erb')].instance_variable_get(:@compiled_method).length.must_equal 1
    body.strip.must_equal '<h1>1</h1>'
    app.render_opts[:cache][File.expand_path('spec/views/about.erb')].instance_variable_get(:@compiled_method).length.must_equal 1
  end

  it "adds support for template precompilation with :inline" do
    app(:bare) do
      plugin :render, :views=>'spec/views'
      plugin :precompile_templates
      route do |r|
        render(:inline=>'a', :cache_key=>'a')
      end
    end

    app.render_opts[:cache]['a'].must_be_nil
    app.precompile_templates :inline=>'a', :cache_key=>'a'
    app.render_opts[:cache]['a'].wont_equal nil
    app.render_opts[:cache]['a'].instance_variable_get(:@compiled_method).length.must_equal 1
    body.strip.must_equal "a"
    app.render_opts[:cache]['a'].instance_variable_get(:@compiled_method).length.must_equal 1
  end
end

describe "precompile_templates plugin - precompile_views method" do 
  it "adds support for template precompilation without locals" do
    app(:bare) do
      plugin :render, :views=>'spec/views', :layout=>'layout-yield'
      plugin :precompile_templates
      route do |r|
        @a = ' 1 '
        view('iv')
      end
    end

    app.render_opts[:cache][File.expand_path('spec/views/iv.erb')].must_be_nil
    app.render_opts[:cache][File.expand_path('spec/views/layout-yield.erb')].must_be_nil
    if Roda::RodaPlugins::Render::COMPILED_METHOD_SUPPORT
      app.render_opts[:template_method_cache]['iv'].must_be_nil
    end

    app.precompile_views ['iv']
    app.freeze_template_caches!
    app.render_opts[:cache][File.expand_path('spec/views/iv.erb')].wont_be_nil
    app.render_opts[:cache][File.expand_path('spec/views/layout-yield.erb')].wont_be_nil
    app.render_opts[:cache].size.must_equal 2
    if Roda::RodaPlugins::Render::COMPILED_METHOD_SUPPORT
      app.render_opts[:template_method_cache]['iv'].wont_be_nil
      app.render_opts[:template_method_cache].size.must_equal 2
    end

    body.strip.gsub(/\s+/, ' ').must_equal 'Header 1 Footer'
    app.render_opts[:cache].size.must_equal 2
    if Roda::RodaPlugins::Render::COMPILED_METHOD_SUPPORT
      app.render_opts[:template_method_cache].size.must_equal 2
    end
  end

  it "adds support for template precompilation with :locals" do
    app(:bare) do
      plugin :render, :views=>'spec/views', :layout=>false
      plugin :precompile_templates
      route do |r|
        render('about', :locals=>{:title=>'1'})
      end
    end

    app.render_opts[:cache][File.expand_path('spec/views/about.erb')].must_be_nil
    if Roda::RodaPlugins::Render::COMPILED_METHOD_SUPPORT
      app.render_opts[:template_method_cache]['about'].must_be_nil
    end

    app.precompile_views ['about'], [:title]
    app.freeze_template_caches!
    app.render_opts[:cache][File.expand_path('spec/views/about.erb')].wont_be_nil
    app.render_opts[:cache].size.must_equal 1
    if Roda::RodaPlugins::Render::COMPILED_METHOD_SUPPORT
      app.render_opts[:template_method_cache][[:_render_locals, "about", [:title]]].wont_be_nil
      app.render_opts[:template_method_cache].size.must_equal 1
    end

    body.strip.must_equal '<h1>1</h1>'
    app.render_opts[:cache].size.must_equal 1
    if Roda::RodaPlugins::Render::COMPILED_METHOD_SUPPORT
      app.render_opts[:template_method_cache].size.must_equal 1
    end
  end
end
end

begin
  require 'tilt/plain'
rescue LoadError
  warn "tilt/plain not installed, skipping precompiled_templates plugin test"  
else
describe "precompile_templates plugin" do 
  it "adds support for template precompilation for tilt template types that do not support precompilation" do
    app(:bare) do
      plugin :render, :views=>'spec/views'
      plugin :precompile_templates
      route do |r|
        render(:path=>File.expand_path('spec/assets/css/app.html'), :template_opts=>{:cache=>false})
      end
    end
    key = [File.expand_path("spec/assets/css/app.html"), nil, nil, {:cache=>false}, nil]
    app.render_opts[:cache][key].must_be_nil
    app.precompile_templates(:path=>File.expand_path('spec/assets/css/app.html'), :template_opts=>{:cache=>false})
    app.render_opts[:cache][key].wont_be_nil
    app.freeze_template_caches!
    body.must_match(/color: red;/)
  end
end
end
