# frozen-string-literal: true

#
class Roda
  module RodaPlugins
    # The view_hierarchy plugin allows you to specify, per request, an
    # ordered list of view directories to search for templates.  When
    # rendering a template, the first directory in the hierarchy that
    # contains the template file wins.  If no directory in the hierarchy
    # contains the template, the default path specified by the render
    # plugin is used.
    #
    # This is useful for applications that need to pick view directories
    # dynamically per request, such as multi-tenant applications using
    # per-tenant view directories, or applications supporting themes,
    # with a common directory as a fallback:
    #
    #   plugin :render
    #   plugin :view_hierarchy
    #
    #   route do |r|
    #     set_view_hierarchy ["views/tenants/#{r.params['tenant']}",
    #                         "views/themes/#{r.params['theme']}",
    #                         'views/common']
    #
    #     # Uses the first of the following paths that exists:
    #     # * views/tenants/TENANT/home.erb
    #     # * views/themes/THEME/home.erb
    #     # * views/common/home.erb
    #     # * views/home.erb
    #     view 'home'
    #   end
    #
    # The hierarchy is stored per request, so nested template rendering
    # (partials and layouts rendered from within a template) uses the
    # same hierarchy as the enclosing template.
    #
    # The hierarchy only affects template lookup.  Once a template file
    # has been selected, it is the file used.  If the selected template
    # raises an error during rendering, the error is raised as usual;
    # the view_hierarchy plugin does not fall back to a template with
    # the same name in a later directory in the hierarchy.
    #
    # The hierarchy is not used if the :views or :path option is
    # explicitly passed to render/view.
    #
    # Each directory in the hierarchy must be allowed by the render
    # plugin's :allowed_paths option (directories under the render
    # plugin's :views directory are allowed by default).
    #
    # The resolved template path is used as the render cache key, so
    # template caching remains correct when different requests use
    # different hierarchies.  The compiled template method cache is
    # disabled for requests using a view hierarchy, since the same
    # template name can resolve to different paths in different
    # requests.
    module ViewHierarchy
      # Depend on the render plugin, since this plugin only makes
      # sense when the render plugin is used.
      def self.load_dependencies(app)
        app.plugin :render
      end

      module InstanceMethods
        # Set the view hierarchy to use for the current request.  The
        # argument should be an array of view directories, searched in
        # order.  Relative directories are expanded relative to the
        # current working directory.  Use nil to clear the hierarchy.
        def set_view_hierarchy(dirs)
          @_view_hierarchy = dirs && dirs.map{|dir| self.class.expand_path(dir, nil)}.freeze
        end

        # The view hierarchy in use for the current request, if any.
        def view_hierarchy
          @_view_hierarchy
        end

        private

        if Render::COMPILED_METHOD_SUPPORT
          # Disable the compiled template method cache when a view
          # hierarchy is in use, since the same template name can
          # resolve to different paths in different requests.
          def _cached_template_method_key(template)
            return if @_view_hierarchy

            super
          end

          # Disable the compiled template method cache when a view
          # hierarchy is in use, since the same template name can
          # resolve to different paths in different requests.
          def _cached_template_method_lookup(method_cache, template)
            return if @_view_hierarchy

            super
          end
        end

        # If a view hierarchy is set and neither the :views nor the
        # :path option was explicitly given, use the path in the first
        # directory of the hierarchy that contains the template file.
        # If no directory in the hierarchy contains the template file,
        # use the default path.
        def template_path(opts)
          if @_view_hierarchy && (!opts[:views] || opts[:views] == render_opts[:views])
            @_view_hierarchy.each do |dir|
              path = super(opts.merge(:views=>dir))
              return path if File.file?(path)
            end
          end

          super
        end
      end
    end

    register_plugin(:view_hierarchy, ViewHierarchy)
  end
end
