require_relative "spec_helper"

describe "Roda.plugin_group" do
  def new_plugin(name, deps: [], conflicts: [], configured: nil)
    plugin = Module.new do
      extend Roda::RodaPlugins::Declarations
    end
    plugin.depends_on(*deps)
    plugin.conflicts_with(*conflicts)
    plugin.define_singleton_method(:name){name}
    if configured
      plugin.define_singleton_method(:configure) do |mod, *args|
        configured << [name, args]
      end
    end
    plugin
  end

  def new_app
    Class.new(Roda)
  end

  it "loads plugins and dependencies in dependency order, configuring duplicates only once" do
    configured = []
    a = new_plugin("a", configured: configured)
    b = new_plugin("b", deps: [a], configured: configured)
    c = new_plugin("c", deps: [a, b], configured: configured)

    app = new_app
    app.plugin_group(c, b, a).must_be_nil

    configured.must_equal [["a", []], ["b", []], ["c", []]]
    app.plugins.must_equal [Roda::RodaPlugins::Base, a, b, c]
  end

  it "passes arguments to configure when entries are arrays" do
    configured = []
    a = new_plugin("a", configured: configured)

    new_app.plugin_group([a, 1, 2])

    configured.must_equal [["a", [1, 2]]]
  end

  it "resolves dependencies given as registered plugin symbols" do
    configured = []
    a = new_plugin("pg_group_spec_a", configured: configured)
    Roda::RodaPlugins.register_plugin(:pg_group_spec_a, a)
    b = new_plugin("b", deps: [:pg_group_spec_a], configured: configured)

    app = new_app
    app.plugin_group(b)

    configured.must_equal [["pg_group_spec_a", []], ["b", []]]
    app.plugins.must_equal [Roda::RodaPlugins::Base, a, b]
  end

  it "raises for missing dependencies before configuring anything, with the dependency chain" do
    configured = []
    a = new_plugin("a", configured: configured)
    b = new_plugin("b", deps: [a, :pg_group_spec_missing], configured: configured)

    app = new_app
    e = proc{app.plugin_group(b)}.must_raise(Roda::RodaError)
    e.message.must_include "pg_group_spec_missing"
    e.message.must_include "b -> pg_group_spec_missing"

    configured.must_equal []
    app.plugins.must_equal [Roda::RodaPlugins::Base]
  end

  it "raises for dependency cycles before configuring anything, with the dependency chain" do
    configured = []
    a = new_plugin("a", configured: configured)
    b = new_plugin("b", deps: [a], configured: configured)
    a.depends_on(b)

    app = new_app
    e = proc{app.plugin_group(a)}.must_raise(Roda::RodaError)
    e.message.must_include "a -> b -> a"

    configured.must_equal []
    app.plugins.must_equal [Roda::RodaPlugins::Base]
  end

  it "raises for conflicts inside the group before configuring anything, with the dependency chain" do
    configured = []
    a = new_plugin("a", configured: configured)
    b = new_plugin("b", conflicts: [a], configured: configured)
    c = new_plugin("c", deps: [b], configured: configured)

    app = new_app
    e = proc{app.plugin_group(a, c)}.must_raise(Roda::RodaError)
    e.message.must_include "conflicts"
    e.message.must_include "c -> b"

    configured.must_equal []
    app.plugins.must_equal [Roda::RodaPlugins::Base]
  end

  it "raises for conflicts with already loaded plugins" do
    configured = []
    a = new_plugin("a", configured: configured)
    b = new_plugin("b", conflicts: [a], configured: configured)

    app = new_app
    app.plugin(a)
    configured.clear

    e = proc{app.plugin_group(b)}.must_raise(Roda::RodaError)
    e.message.must_include "conflicts"

    configured.must_equal []
    app.plugins.must_equal [Roda::RodaPlugins::Base, a]
  end

  it "raises for conflicts declared by already loaded plugins against the group" do
    configured = []
    a = new_plugin("a", configured: configured)
    b = new_plugin("b", configured: configured)
    a.conflicts_with(b)

    app = new_app
    app.plugin(a)
    configured.clear

    e = proc{app.plugin_group(b)}.must_raise(Roda::RodaError)
    e.message.must_include "conflicts"

    configured.must_equal []
    app.plugins.must_equal [Roda::RodaPlugins::Base, a]
  end

  it "treats plugins loaded by the superclass as loaded, without modifying the superclass" do
    configured = []
    a = new_plugin("a", configured: configured)
    b = new_plugin("b", deps: [a], configured: configured)

    parent = new_app
    parent.plugin(a)
    configured.must_equal [["a", []]]

    child = Class.new(parent)
    child.plugin_group(b)

    configured.must_equal [["a", []], ["b", []]]
    child.plugins.must_equal [Roda::RodaPlugins::Base, a, b]
    parent.plugins.must_equal [Roda::RodaPlugins::Base, a]
  end

  it "checks conflicts against plugins loaded by the superclass" do
    a = new_plugin("a")
    b = new_plugin("b", conflicts: [a])

    parent = new_app
    parent.plugin(a)
    child = Class.new(parent)

    e = proc{child.plugin_group(b)}.must_raise(Roda::RodaError)
    e.message.must_include "conflicts"

    child.plugins.must_equal [Roda::RodaPlugins::Base, a]
    parent.plugins.must_equal [Roda::RodaPlugins::Base, a]
  end

  it "leaves independent applications unaffected after a failed group load" do
    configured = []
    bad = new_plugin("bad", deps: [:pg_group_spec_missing], configured: configured)
    good = new_plugin("good", configured: configured)

    app1 = new_app
    app2 = new_app

    proc{app1.plugin_group(bad, good)}.must_raise(Roda::RodaError)

    app1.plugins.must_equal [Roda::RodaPlugins::Base]
    app2.plugins.must_equal [Roda::RodaPlugins::Base]
    configured.must_equal []

    app2.plugin_group(good)
    configured.must_equal [["good", []]]
    app2.plugins.must_equal [Roda::RodaPlugins::Base, good]
    app1.plugins.must_equal [Roda::RodaPlugins::Base]

    app1.plugin_group(good)
    app1.plugins.must_equal [Roda::RodaPlugins::Base, good]
  end

  it "raises when adding a plugin group to a frozen class" do
    app = new_app
    app.freeze
    proc{app.plugin_group(new_plugin("a"))}.must_raise(Roda::RodaError)
  end

  it "raises for invalid plugin types in the group" do
    proc{new_app.plugin_group("a")}.must_raise(Roda::RodaError)
  end
end
