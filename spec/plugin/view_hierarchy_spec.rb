require_relative "../spec_helper"

begin
  require 'tilt'
  require 'tilt/erb'
  require_relative '../../lib/roda/plugins/render'
rescue LoadError
  warn "tilt not installed, skipping view_hierarchy plugin test"
else
describe "view_hierarchy plugin" do
  it "resolves the template path using the first hierarchy directory containing the file" do
    app(:bare) do
      plugin :render, :views=>'spec/views'
      plugin :view_hierarchy
      route do |r|
        r.on String, String do |tenant, page|
          set_view_hierarchy ["spec/views/hierarchy/#{tenant}", 'spec/views/hierarchy/common']
          find_template(:template=>page)[:path]
        end
      end
    end

    # In both tenant and common, tenant wins
    body('/tenant_a/home').must_equal File.expand_path('spec/views/hierarchy/tenant_a/home.erb')

    # Not in tenant, falls back to common
    body('/tenant_b/home').must_equal File.expand_path('spec/views/hierarchy/common/home.erb')
    body('/tenant_a/note_page').must_equal File.expand_path('spec/views/hierarchy/common/note_page.erb')

    # Not in any hierarchy directory, falls back to the default :views path
    body('/tenant_a/a').must_equal File.expand_path('spec/views/a.erb')
    body('/tenant_a/bogus').must_equal File.expand_path('spec/views/bogus.erb')
  end

  it "renders the first existing template in the hierarchy" do
    app(:bare) do
      plugin :render, :views=>'spec/views'
      plugin :view_hierarchy
      route do |r|
        r.on 'render', String, String do |tenant, page|
          set_view_hierarchy ["spec/views/hierarchy/#{tenant}", 'spec/views/hierarchy/common']
          render page
        end
      end
    end

    body('/render/tenant_a/home').delete("\n").must_equal 'tenant_a home'
    body('/render/tenant_b/home').delete("\n").must_equal 'common home'
  end

  it "uses the same hierarchy for nested partials" do
    app(:bare) do
      plugin :render, :views=>'spec/views'
      plugin :view_hierarchy
      route do |r|
        r.on String, String do |tenant, page|
          set_view_hierarchy ["spec/views/hierarchy/#{tenant}", 'spec/views/hierarchy/common']
          render page
        end
      end
    end

    # note_page only exists in common, but the nested _note partial
    # is still resolved using the same hierarchy
    body('/tenant_a/note_page').delete("\n").must_equal 'page[note from tenant_a]'
    body('/tenant_b/note_page').delete("\n").must_equal 'page[note from common]'
  end

  it "uses the hierarchy for the layout used by view" do
    app(:bare) do
      plugin :render, :views=>'spec/views'
      plugin :view_hierarchy
      route do |r|
        r.on String, String do |tenant, page|
          set_view_hierarchy ["spec/views/hierarchy/#{tenant}", 'spec/views/hierarchy/common']
          view page
        end
      end
    end

    # layout.erb only exists in common
    body('/tenant_a/home').delete("\n").must_equal 'layout(tenant_a home)'
    body('/tenant_b/home').delete("\n").must_equal 'layout(common home)'
  end

  it "does not fall back to another hierarchy directory when the selected template raises" do
    app(:bare) do
      plugin :render, :views=>'spec/views'
      plugin :view_hierarchy
      route do |r|
        r.on String, String do |tenant, page|
          set_view_hierarchy ["spec/views/hierarchy/#{tenant}", 'spec/views/hierarchy/common']
          render page
        end
      end
    end

    # broken_page only exists in common, but the nested _broken partial
    # exists in both tenant_a and common.  The tenant_a partial is
    # selected, and its error is raised instead of silently falling
    # back to the common partial.
    e = proc{body('/tenant_a/broken_page')}.must_raise(RuntimeError)
    e.message.must_equal 'boom from tenant_a partial'
    e.backtrace.first.must_include 'tenant_a/_broken.erb'

    e = proc{body('/tenant_b/broken_page')}.must_raise(RuntimeError)
    e.message.must_equal 'boom from common partial'
    e.backtrace.first.must_include 'common/_broken.erb'
  end

  it "handles alternating hierarchies when template caching is enabled" do
    app(:bare) do
      plugin :render, :views=>'spec/views'
      plugin :view_hierarchy
      route do |r|
        r.on String, String do |tenant, page|
          set_view_hierarchy ["spec/views/hierarchy/#{tenant}", 'spec/views/hierarchy/common']
          render page
        end
      end
    end

    # The second round is served from the render cache, keyed by the
    # resolved template path.
    2.times do
      body('/tenant_a/home').delete("\n").must_equal 'tenant_a home'
      body('/tenant_b/home').delete("\n").must_equal 'common home'
      body('/tenant_a/note_page').delete("\n").must_equal 'page[note from tenant_a]'
      body('/tenant_b/note_page').delete("\n").must_equal 'page[note from common]'
    end

    # The compiled template method cache is bypassed when a hierarchy
    # is set, so it must not hold an entry for the template name.
    app.render_opts[:template_method_cache]['home'].must_be_nil if Roda::RodaPlugins::Render::COMPILED_METHOD_SUPPORT
  end

  it "is not used when the :views or :path option is explicitly given" do
    app(:bare) do
      plugin :render, :views=>'spec/views'
      plugin :view_hierarchy
      route do |r|
        set_view_hierarchy ['spec/views/hierarchy/tenant_a']
        r.is 'views' do
          render 'home', :views=>'spec/views/hierarchy/common'
        end
        r.is 'path' do
          render 'home', :path=>File.expand_path('spec/views/hierarchy/common/home.erb')
        end
      end
    end

    body('/views').delete("\n").must_equal 'common home'
    body('/path').delete("\n").must_equal 'common home'
  end

  it "stores expanded directories per request and supports clearing the hierarchy" do
    app(:bare) do
      plugin :render, :views=>'spec/views'
      plugin :view_hierarchy
      route do |r|
        set_view_hierarchy ['spec/views/hierarchy/common']
        dirs = view_hierarchy
        frozen = dirs.frozen?
        set_view_hierarchy nil
        [dirs, frozen, view_hierarchy].inspect
      end
    end

    body('/').must_equal [[File.expand_path('spec/views/hierarchy/common')], true, nil].inspect
  end
end
end
