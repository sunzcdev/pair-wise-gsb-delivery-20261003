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

    # Stores the registered name for each registered plugin module
    @plugin_names = {}

    class << self
      # Make warn a public method, as it is used for deprecation warnings.
      # Roda::RodaPlugins.warn can be overridden for custom handling of
      # deprecation warnings.
      public :warn
    end

    # Methods that plugin modules can extend themselves with to declare
    # dependencies on other plugins and conflicts with other plugins:
    #
    #   module MyPlugin
    #     extend Roda::RodaPlugins::Declarations
    #
    #     depends_on :render
    #     conflicts_with :json
    #   end
    #
    # Declared dependencies and conflicts are resolved and checked when
    # loading a group of plugins at once using Roda.plugin_group.
    module Declarations
      # Declare that this plugin depends on the given plugins.  Each
      # entry can be a symbol for a registered plugin, or a plugin
      # module.
      def depends_on(*deps)
        plugin_dependencies.concat(deps)
      end

      # Declare that this plugin cannot be used together with the
      # given plugins.  Each entry can be a symbol for a registered
      # plugin, or a plugin module.
      def conflicts_with(*plugins)
        plugin_conflicts.concat(plugins)
      end

      # The plugins this plugin depends on.
      def plugin_dependencies
        @plugin_dependencies ||= []
      end

      # The plugins this plugin conflicts with.
      def plugin_conflicts
        @plugin_conflicts ||= []
      end
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
      @plugin_names[mod] = name
    end

    # The dependencies declared by the given plugin module, or an empty
    # array if the plugin does not declare dependencies.
    def self.plugin_dependencies(mod)
      mod.respond_to?(:plugin_dependencies) ? mod.plugin_dependencies : EMPTY_ARRAY
    end

    # The conflicts declared by the given plugin module, or an empty
    # array if the plugin does not declare conflicts.
    def self.plugin_conflicts(mod)
      mod.respond_to?(:plugin_conflicts) ? mod.plugin_conflicts : EMPTY_ARRAY
    end

    # A human readable name for the given plugin module, for use in
    # error messages.
    def self.plugin_display_name(mod)
      if name = @plugin_names[mod]
        name.to_s
      else
        (mod.name || mod.inspect).to_s
      end
    end

    # Resolve the given requested plugins into a load order, taking into
    # account the plugins already loaded into the Roda class and the
    # dependencies declared by the plugins.  Each requested entry can be
    # a plugin module, a symbol for a registered plugin, or an array
    # containing a plugin module or symbol followed by arguments for the
    # plugin's configure method.
    #
    # Returns an array of [plugin module, arguments] pairs, ordered so
    # that all dependencies of a plugin appear before the plugin itself,
    # with duplicates and already loaded plugins removed, so each plugin
    # is configured only once.
    #
    # Raises a RodaError if a declared dependency cannot be loaded, if
    # there is a dependency cycle, or if a plugin in the group conflicts
    # with another plugin in the group or with an already loaded plugin.
    # As this raises before returning, no plugin in the group will have
    # been loaded into the class when it raises.
    def self.resolve_plugin_group(loaded_plugins, requested)
      order = []
      resolved = {}
      loaded = {}
      chains = {}
      arguments = {}
      loaded_plugins.each{|mod| loaded[mod] = true}

      visit = lambda do |mod, chain|
        next if loaded[mod] || resolved[mod]

        if chain.include?(mod)
          cycle = chain[chain.index(mod)..-1] + [mod]
          raise RodaError, "plugin dependency cycle detected: #{cycle.map{|m| plugin_display_name(m)}.join(' -> ')}"
        end

        chain = chain + [mod]
        chains[mod] ||= chain

        plugin_dependencies(mod).each do |dep|
          visit.call(resolve_plugin_dependency(dep, chain), chain)
        end

        resolved[mod] = true
        order << mod
      end

      requested.each do |entry|
        entry = [entry] unless entry.is_a?(Array)
        mod = entry[0]
        mod = load_plugin(mod) if mod.is_a?(Symbol)
        raise RodaError, "Invalid plugin type: #{mod.class.inspect}" unless mod.is_a?(Module)
        arguments[mod] = entry[1..-1] unless arguments.has_key?(mod)
        visit.call(mod, [])
      end

      check_plugin_group_conflicts(loaded_plugins, order, chains)

      order.map{|mod| [mod, arguments[mod] || EMPTY_ARRAY]}
    end

    # Resolve a single declared plugin dependency to a plugin module,
    # raising a RodaError including the dependency chain if the
    # dependency cannot be resolved.
    def self.resolve_plugin_dependency(dep, chain)
      case dep
      when Module
        dep
      when Symbol
        begin
          load_plugin(dep)
        rescue LoadError, RodaError
          raise RodaError, "plugin #{plugin_display_name(chain[-1])} depends on plugin #{dep}, which could not be loaded (dependency chain: #{(chain.map{|m| plugin_display_name(m)} + [dep.to_s]).join(' -> ')})"
        end
      else
        raise RodaError, "invalid plugin dependency #{dep.inspect} declared by plugin #{plugin_display_name(chain[-1])}"
      end
    end

    # Check that none of the plugins in the resolved group conflict with
    # each other or with the already loaded plugins, raising a RodaError
    # including the dependency chain if a conflict is found.
    def self.check_plugin_group_conflicts(loaded_plugins, order, chains)
      present = {}
      loaded_plugins.each{|mod| present[mod] = true}
      order.each{|mod| present[mod] = true}

      check = lambda do |mod, against, chain|
        plugin_conflicts(mod).each do |conflict|
          conflict = @plugins[conflict] if conflict.is_a?(Symbol)
          next unless conflict.is_a?(Module) && conflict != mod && against[conflict]

          message = "plugin #{plugin_display_name(mod)} conflicts with plugin #{plugin_display_name(conflict)}"
          message += " (dependency chain: #{chain.map{|m| plugin_display_name(m)}.join(' -> ')})" if chain
          raise RodaError, message
        end
      end

      new_plugins = {}
      order.each{|mod| new_plugins[mod] = true}

      loaded_plugins.each{|mod| check.call(mod, new_plugins, nil)}
      order.each{|mod| check.call(mod, present, chains[mod])}
      nil
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
