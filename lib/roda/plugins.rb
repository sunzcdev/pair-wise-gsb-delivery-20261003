# frozen-string-literal: true

require_relative "cache"

class Roda
  # Module in which all Roda plugins should be stored. Also contains logic for
  # registering and loading plugins.
  module RodaPlugins
    OPTS = {}.freeze
    EMPTY_ARRAY = [].freeze

    # Stores registered plugins
    @plugins = RodaCache.new

    class << self
      # Make warn a public method, as it is used for deprecation warnings.
      # Roda::RodaPlugins.warn can be overridden for custom handling of
      # deprecation warnings.
      public :warn
    end

    # If the registered plugin already exists, use it.  Otherwise,
    # require it and return it.  This raises a LoadError if such a
    # plugin doesn't exist, or a RodaError if it exists but it does
    # not register itself correctly.
    def self.load_plugin(name)
      h = @plugins
      unless plugin = h[name]
        require "roda/plugins/#{name}"
        raise RodaError, "Plugin #{name} did not register itself correctly in Roda::RodaPlugins" unless plugin = h[name]
      end
      plugin
    end

    # Define an attr_reader method for name that uses the instance variable
    # <tt>@opt_*</tt>. Designed to be used with instance variables set via RodaOpts#[]=.
    def self.opt_attr_reader(mod, key, name: key)
      opt_name = :"opt_#{key}"
      mod.send(:attr_reader, opt_name)
      mod.send(:alias_method, name, opt_name)
      mod.send(:remove_method, opt_name)
      nil
    end

    # Register the given plugin with Roda, so that it can be loaded using #plugin
    # with a symbol.  Should be used by plugin files. Example:
    #
    #   Roda::RodaPlugins.register_plugin(:plugin_name, PluginModule)
    def self.register_plugin(name, mod)
      @plugins[name] = mod
    end

    # Plan the loading of a group of plugins into a Roda class.  +loaded+
    # is an array of plugin modules already loaded into the class, and
    # +plugins+ is an array of plugin symbols and/or modules to load.
    #
    # Returns an array of the plugin modules to load, in dependency order,
    # excluding plugins that are already loaded.  Plugins can declare
    # dependencies on and conflicts with other plugins by defining
    # +plugin_dependencies+ and +plugin_conflicts+ methods that return
    # arrays of plugin symbols and/or modules.
    #
    # Raises a RodaError if a declared dependency cannot be loaded, if
    # there is a dependency cycle, or if a plugin in the group conflicts
    # with another plugin in the group or with an already loaded plugin.
    # The error message includes the dependency chain related to the
    # failure.  This does not modify the Roda class, so no plugin is
    # configured if an error is raised.
    def self.plan_plugins(loaded, plugins)
      order = []
      chains = {}
      state = {}

      visit = lambda do |ref, chain|
        plugin = if ref.is_a?(Symbol)
          begin
            load_plugin(ref)
          rescue LoadError
            raise RodaError, "plugin dependency #{ref.inspect} could not be loaded (dependency chain: #{plugin_chain(chain.map{|r, _| r} + [ref])})"
          end
        else
          ref
        end
        raise RodaError, "Invalid plugin type: #{plugin.class.inspect}" unless plugin.is_a?(Module)

        next if loaded.include?(plugin) || state[plugin] == :done

        if state[plugin] == :visiting
          cycle = chain[chain.index{|_, mod| mod.equal?(plugin)}..-1].map{|r, _| r}
          raise RodaError, "plugin dependency cycle detected: #{plugin_chain(cycle + [ref])}"
        end

        state[plugin] = :visiting
        chain += [[ref, plugin]]
        chains[plugin] = chain.map{|r, _| r}
        declared_plugin_dependencies(plugin).each{|dep| visit.call(dep, chain)}
        state[plugin] = :done
        order << plugin
      end

      plugins.each{|plugin| visit.call(plugin, EMPTY_ARRAY)}

      all = loaded + order
      order.each{|plugin| check_plugin_conflicts(plugin, all, chains)}
      loaded.each{|plugin| check_plugin_conflicts(plugin, order, chains)}

      order
    end

    # The dependencies declared by the plugin via a plugin_dependencies
    # method, as an array of plugin symbols and/or modules.
    def self.declared_plugin_dependencies(plugin)
      if plugin.respond_to?(:plugin_dependencies)
        plugin.plugin_dependencies
      else
        EMPTY_ARRAY
      end
    end

    # The conflicts declared by the plugin via a plugin_conflicts
    # method, as an array of plugin symbols and/or modules.
    def self.declared_plugin_conflicts(plugin)
      if plugin.respond_to?(:plugin_conflicts)
        plugin.plugin_conflicts
      else
        EMPTY_ARRAY
      end
    end

    # Raise a RodaError if the plugin declares a conflict with any of
    # the other plugins.  +chains+ maps planned plugins to the
    # dependency chain through which they were pulled in, for use in
    # error messages.
    def self.check_plugin_conflicts(plugin, others, chains)
      declared_plugin_conflicts(plugin).each do |ref|
        conflict = if ref.is_a?(Symbol)
          begin
            load_plugin(ref)
          rescue LoadError
            nil
          end
        else
          ref
        end
        next if conflict.nil? || conflict.equal?(plugin) || !others.include?(conflict)

        chain = chains[plugin] || chains[conflict] || [plugin]
        raise RodaError, "plugin conflict detected: #{plugin_name(plugin)} conflicts with #{plugin_name(ref)} (dependency chain: #{plugin_chain(chain)})"
      end
      nil
    end

    # A string representing the plugin, for use in error messages.
    def self.plugin_name(plugin)
      if plugin.is_a?(Module) && (name = plugin.name)
        name
      else
        plugin.inspect
      end
    end

    # A string representing a dependency chain of plugin symbols
    # and/or modules, for use in error messages.
    def self.plugin_chain(chain)
      chain.map{|plugin| plugin_name(plugin)}.join(' -> ')
    end

    # Deprecate the constant with the given name in the given module,
    # if the ruby version supports it.
    def self.deprecate_constant(mod, name)
      # simplecov:disable
      if RUBY_VERSION >= '2.3'
        mod.deprecate_constant(name)
      end
      # simplecov:enable
    end

    if RUBY_VERSION >= '3.3'
      # Create a new module using the block, and set the temporary name
      # on it using the given a containing module and name.
      def self.set_temp_name(mod)
        mod.set_temporary_name(yield)
        mod
      end
    # simplecov:disable
    else
      def self.set_temp_name(mod)
        mod
      end
    end
    # simplecov:enable
  end
end
