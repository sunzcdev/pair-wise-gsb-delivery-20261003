require_relative "../spec_helper"

begin
  require 'tilt'
  require 'tilt/erb'
  require 'tilt/string'
  require_relative '../../lib/roda/plugins/render'
rescue LoadError
  warn "tilt not installed, skipping view_hierarchy plugin test"
else
describe "view_hierarchy plugin" do
  def hierarchy_app
    app(:bare) do
      plugin :render, :views=>'spec/views'
      plugin :view_hierarchy

      route do |r|
        r.on "tenant" do
          set_view_hierarchy %w[hierarchy/tenant hierarchy/theme hierarchy/common]
          r.is("welcome"){render 'welcome'}
          r.is("oops"){render 'oops'}
          r.is("only_common"){render 'only_common'}
        end

        r.on "theme" do
          set_view_hierarchy %w[hierarchy/theme hierarchy/common]
          r.is("welcome"){render 'welcome'}
        end

        r.on "common" do
          set_view_hierarchy %w[hierarchy/common]
          r.is("welcome"){render 'welcome'}
          r.is("oops"){render 'oops'}
        end

      end
    end
  end

  it "uses the first template in the hierarchy that exists" do
    hierarchy_app
    body('/tenant/welcome').must_equal "TENANT[tenant-shared\n]\n"
    body('/theme/welcome').must_equal "THEME[theme-shared\n]\n"
    body('/common/welcome').must_equal "COMMON[common-shared\n]\n"
  end

  it "uses the default views directory for templates not in any hierarchy directory" do
    app(:bare) do
      plugin :render, :views=>'spec/views'
      plugin :view_hierarchy
      route do |r|
        r.is("with") do
          set_view_hierarchy %w[hierarchy/tenant hierarchy/common]
          render 'a'
        end
        r.is("without"){render 'a'}
      end
    end

    body('/with').must_equal "a\n"
    body('/without').must_equal "a\n"
  end

  it "uses later hierarchy directories for templates missing from earlier ones" do
    hierarchy_app
    body('/tenant/only_common').must_equal "only-common\n"
  end

  it "uses the same hierarchy for nested partials" do
    hierarchy_app
    body('/tenant/welcome').must_include 'tenant-shared'
    body('/theme/welcome').must_include 'theme-shared'
    body('/common/welcome').must_include 'common-shared'
  end

  it "does not fall back to a later hierarchy template when the selected template raises" do
    hierarchy_app
    e = _{req('/tenant/oops')}.must_raise RuntimeError
    e.message.must_equal 'tenant broken partial'
    e.backtrace.first.must_include 'hierarchy/tenant/_broken.erb'

    # The common version of the same template still renders fine
    body('/common/oops').must_equal "common-oops-ok\n"
  end

  it "handles alternating requests with different hierarchies when caching is enabled" do
    hierarchy_app
    2.times do
      body('/tenant/welcome').must_equal "TENANT[tenant-shared\n]\n"
      body('/theme/welcome').must_equal "THEME[theme-shared\n]\n"
      body('/common/welcome').must_equal "COMMON[common-shared\n]\n"
      body('/common/oops').must_equal "common-oops-ok\n"
      _{req('/tenant/oops')}.must_raise RuntimeError
    end
  end

  it "supports a :view_hierarchy render option for a single render call" do
    app(:bare) do
      plugin :render, :views=>'spec/views'
      plugin :view_hierarchy
      route do |r|
        r.is("tenant"){render 'ping', :view_hierarchy=>%w[hierarchy/tenant hierarchy/common]}
        r.is("common"){render 'ping', :view_hierarchy=>%w[hierarchy/common]}
      end
    end

    body('/tenant').must_equal "tenant-ping\n"
    body('/common').must_equal "common-ping\n"
  end

  it "supports setting the hierarchy via the view_options plugin" do
    app(:bare) do
      plugin :render, :views=>'spec/views'
      plugin :view_options
      plugin :view_hierarchy
      route do |r|
        r.on "tenant" do
          set_view_options :view_hierarchy=>%w[hierarchy/tenant hierarchy/common]
          r.is("welcome"){render 'welcome'}
        end
        r.is("common"){render 'ping', :view_hierarchy=>%w[hierarchy/common]}
      end
    end

    body('/tenant/welcome').must_equal "TENANT[tenant-shared\n]\n"
    body('/common').must_equal "common-ping\n"
  end

  it "supports a default hierarchy passed when loading the plugin" do
    app(:bare) do
      plugin :render, :views=>'spec/views'
      plugin :view_hierarchy, %w[hierarchy/theme hierarchy/common]
      route do |r|
        r.is("welcome"){render 'welcome'}
        r.is("tenant") do
          set_view_hierarchy %w[hierarchy/tenant hierarchy/common]
          render 'welcome'
        end
      end
    end

    body('/welcome').must_equal "THEME[theme-shared\n]\n"
    body('/tenant').must_equal "TENANT[tenant-shared\n]\n"
    body('/welcome').must_equal "THEME[theme-shared\n]\n"
  end

  it "supports absolute paths in the hierarchy if inside allowed_paths" do
    app(:bare) do
      plugin :render, :views=>'spec/views'
      plugin :view_hierarchy
      route do |r|
        set_view_hierarchy [File.expand_path('spec/views/hierarchy/tenant'), 'hierarchy/common']
        render 'welcome'
      end
    end

    body.must_equal "TENANT[tenant-shared\n]\n"
  end
end
end
