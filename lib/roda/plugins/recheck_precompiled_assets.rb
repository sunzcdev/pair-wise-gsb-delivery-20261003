# frozen-string-literal: true

#
class Roda
  module RodaPlugins
    # The recheck_precompiled_assets plugin enables checking for the precompiled asset metadata file.
    # You need to have already loaded the assets plugin with the +:precompiled+ option and the file
    # specified by the +:precompiled+ option must already exist in order to use the
    # recheck_precompiled_assets plugin.
    #
    # Any time you want to check whether the precompiled asset metadata file has changed and should be
    # reloaded, you can call the +recheck_precompiled_assets+ class method.  This method will check
    # whether the file has changed, and reload it if it has.  If you want to check for modifications on
    # every request, you can use +self.class.recheck_precompiled_assets+ inside your route block.
    #
    # Because the preload hints (e.g. from the assets_preloading plugin) and the static asset routes are
    # derived from the same metadata, swapping them must be atomic.  Otherwise an HTML preload hint could
    # reference a URL whose static route is still using the old metadata (or vice versa).  This plugin
    # swaps the compiled asset metadata and the asset route matchers while holding a single mutex, so the
    # hints and the routes always observe the same version.
    #
    # ==== Retained Assets
    #
    # A newly written metadata file is not trusted blindly.  Before it is applied, every key that must
    # remain serviceable is checked against it:
    #
    # * By default, all keys of the currently active metadata are retained, so a new manifest that drops
    #   an asset is rejected and the previous manifest keeps being used.
    # * You can restrict the check to an explicit set with the +:retained_assets+ option when loading the
    #   plugin.  Each entry is an asset type (<tt>:css</tt>/<tt>:js</tt>) or an asset group given as an
    #   array (e.g. <tt>[:js, :head]</tt>).  Old, already generated URLs for those retained assets keep
    #   being served after the switch because the route matchers accept any digest.
    #
    # +recheck_precompiled_assets+ returns whether the metadata was swapped (+true+) or the previous
    # metadata was kept (+false+), so callers can detect a rejected manifest.
    module RecheckPrecompiledAssets
      # Thread safe wrapper for the compiled asset metadata hash.  Does not wrap all
      # hash methods, only a few that are used.
      class CompiledAssetsHash
        include Enumerable

        def initialize
          @hash = {}
          @mutex = Mutex.new
        end

        def [](key)
          @mutex.synchronize{@hash[key]}
        end

        def []=(key, value)
          @mutex.synchronize{@hash[key] = value}
        end

        def replace(hash)
          hash = hash.instance_variable_get(:@hash) if (CompiledAssetsHash === hash)
          @mutex.synchronize{@hash.replace(hash)}
          self
        end

        def each(&block)
          @mutex.synchronize{@hash.dup}.each(&block)
          self
        end

        def to_json(*args)
          @mutex.synchronize{@hash.dup}.to_json(*args)
        end
      end

      def self.load_dependencies(app, opts=OPTS)
        unless app.respond_to?(:assets_config) && app.assets_config.precompiled
          raise RodaError, "must load assets plugin with precompiled option before loading recheck_precompiled_assets plugin"
        end
        # Set mutex before overriding a method that uses it.  The same mutex protects both the
        # compiled asset metadata and the asset route matchers, so they can be swapped atomically.
        app::RodaRequest.instance_variable_set(:@_precompiled_assets_mutex, Mutex.new)
        app::RodaRequest.instance_variable_set(:@_retained_asset_keys, _retained_asset_keys(opts))
      end

      # Normalize the :retained_assets option into compiled metadata keys.  Asset types
      # (:css/:js) map directly to their string form, while asset groups given as arrays
      # (e.g. [:js, :head]) map to the dotted key used in the metadata (e.g. "js.head").
      def self._retained_asset_keys(opts)
        Array(opts[:retained_assets]).map do |asset|
          case asset
          when Array
            type, *dirs = asset
            dirs.empty? ? type.to_s : "#{type}.#{dirs.join('.')}"
          else
            asset.to_s
          end
        end
      end

      def self.configure(app, opts=OPTS)
        precompiled_file = app.assets_config.precompiled
        prev_mtime = ::File.mtime(precompiled_file)

        request_class = app::RodaRequest
        mutex = request_class.instance_variable_get(:@_precompiled_assets_mutex)
        if opts.key?(:retained_assets)
          request_class.instance_variable_set(:@_retained_asset_keys, _retained_asset_keys(opts))
        end
        retained_keys = request_class.instance_variable_get(:@_retained_asset_keys)
        # Unset the assets matchers, in case a frozen value is already set
        request_class.instance_variable_set(:@assets_matchers, nil)
        # Ensure the assets matchers is set and an unfrozen array, and get the underlying value
        request_class.assets_matchers
        assets_matchers = request_class.instance_variable_get(:@assets_matchers)

        app.instance_exec do
          _update_compiled_assets(_compiled_assets_initial_hash.replace(assets_config.compiled))

          define_singleton_method(:recheck_precompiled_assets) do
            new_mtime = ::File.mtime(precompiled_file)
            if new_mtime != prev_mtime
              new_metadata = _precompiled_asset_metadata(precompiled_file)

              # Determine the keys that must still be present after the switch.  By default every
              # currently active key is retained; with :retained_assets only the configured subset is.
              current_keys = assets_config.compiled.map { |key, _| key }
              required_keys = if retained_keys.empty?
                current_keys
              else
                current_keys & retained_keys
              end

              if (missing = required_keys - new_metadata.map { |key, _| key }).empty?
                # Build the matchers before taking the lock, then swap the metadata and the matchers
                # together, so preload hints and static routes never observe different versions.
                new_assets_matchers = request_class.send(:_build_assets_matchers_for, new_metadata)
                mutex.synchronize do
                  prev_mtime = new_mtime
                  assets_config.compiled.replace(new_metadata)
                  assets_matchers.replace(new_assets_matchers)
                end
                true
              else
                RodaPlugins.warn("recheck_precompiled_assets: keeping previous precompiled metadata, new metadata missing keys: #{missing.join(', ')}")
                false
              end
            else
              false
            end
          end
          singleton_class.send(:alias_method, :recheck_precompiled_assets, :recheck_precompiled_assets)
        end
      end

      module ClassMethods
        private

        # Wrap the precompiled asset metadata in a thread-safe hash.
        def _precompiled_asset_metadata(file)
          CompiledAssetsHash.new.replace(super)
        end

        # Use a thread-safe wrapper of a hash for the :compiled assets option, since
        # the recheck_precompiled_asset_metadata can modify it at runtime.
        def _compiled_assets_initial_hash
          CompiledAssetsHash.new
        end
      end

      module RequestClassMethods
        # Synchronize access and return a dup, so that replacements of the
        # assets matchers at runtime doesn't cause thread safety issues.
        def assets_matchers
          @_precompiled_assets_mutex.synchronize{super.dup}
        end

        private

        # Use a unfrozen array for the assets matchers, so they can be replaced
        # at runtime.
        def _build_assets_matchers
          super.dup
        end

        # Build the asset matchers for the given candidate metadata hash, without changing
        # the application's current configuration.  This allows a new metadata file to be
        # validated and its matchers prepared before the atomic swap is performed.
        def _build_assets_matchers_for(metadata)
          config = roda_class.assets_config
          [:css, :js].map do |t|
            if regexp = _candidate_assets_regexp(t, config, metadata)
              [t, regexp].freeze
            end
          end.compact
        end

        # The regexp matcher for the given asset type using the candidate metadata.
        def _candidate_assets_regexp(type, config, metadata)
          assets = metadata.
            select { |k, _| k =~ /\A#{type}/ }.
            map { |k, md| _asset_regexp(type, k, md) }
          return if assets.empty?
          /#{config[:"compiled_#{type}_prefix"]}(#{Regexp.union(assets)})/
        end

        # Use a regexp that matches any digest.  When the precompiled asset metadata
        # file is updated, this allows requests for a previous precompiled asset to
        # still work.
        def _asset_regexp(type, key, _)
          /#{Regexp.escape(key.sub(/\A#{type}/, ''))}\.[0-9a-fA-F]+\.#{type}/
        end
      end
    end

    register_plugin(:recheck_precompiled_assets, RecheckPrecompiledAssets)
  end
end
