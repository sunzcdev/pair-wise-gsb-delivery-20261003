# frozen-string-literal: true

#
class Roda
  module RodaPlugins
    # The view_hierarchy plugin allows each request to use an ordered list of
    # view directories (a view hierarchy) when looking up templates, such as a
    # tenant directory, a theme directory, and a common directory.  When
    # rendering a template, the directories in the hierarchy are checked in
    # order, and the first template file that exists in the file system is
    # used.  If the template does not exist in any hierarchy directory, the
    # render plugin's default :views directory is used.
    #
    # The hierarchy is only used to find the template file.  Once a template
    # file has been selected, it is used as is.  If the selected template
    # raises an error during rendering, the error is propagated; the plugin
    # does not fall back to a template with the same name in a later
    # hierarchy directory.
    #
    # Because the hierarchy is stored per request, nested template rendering
    # (such as partials rendered inside a template) uses the same hierarchy
    # as the enclosing template.
    #
    # Example:
    #
    #   plugin :render, views: 'views'
    #   plugin :view_hierarchy
    #
    #   route do |r|
    #     r.on "tenants", String do |tenant|
    #       set_view_hierarchy ["tenants/#{tenant}", "themes/default", "common"]
    #       # view 'page' checks the following in order, using the first
    #       # template file that exists:
    #       # * views/tenants/#{tenant}/page.erb
    #       # * views/themes/default/page.erb
    #       # * views/common/page.erb
    #       # * views/page.erb
    #     end
    #   end
    #
    # Hierarchy directories are resolved relative to the render plugin's
    # :views directory, unless they are absolute paths.  Absolute paths must
    # be inside one of the render plugin's :allowed_paths.
    #
    # You can set a default hierarchy for all requests by passing it when
    # loading the plugin:
    #
    #   plugin :view_hierarchy, %w[themes/default common]
    #
    # In addition to set_view_hierarchy, which sets the hierarchy for the
    # remainder of the request, you can pass a :view_hierarchy option to
    # view/render to use a hierarchy for a single render call:
    #
    #   view 'page', view_hierarchy: %w[tenants/a common]
    #
    # When the view_options plugin is also loaded, you can use
    # set_view_options to set the hierarchy for a branch:
    #
    #   set_view_options view_hierarchy: %w[tenants/a common]
    #
    # The hierarchy is included in the render plugin's template cache keys,
    # so requests using different hierarchies can be handled correctly even
    # when template caching is enabled.
    module ViewHierarchy
      # Depend on the render plugin, since this plugin only makes
      # sense when the render plugin is used.
      def self.load_dependencies(app, *)
        app.plugin :render
      end

      # Set the default view hierarchy, used by requests that do not set
      # their own view hierarchy.
      def self.configure(app, hierarchy=nil)
        if hierarchy
          app.opts[:view_hierarchy] = hierarchy.map{|dir| dir.freeze}.freeze
        end
      end

      module ClassMethods
        RodaPlugins.opt_attr_reader(self, :view_hierarchy)
      end

      module InstanceMethods
        # The view hierarchy in effect for the current request, either the
        # hierarchy set via set_view_hierarchy, or the default hierarchy
        # given when loading the plugin.
        def view_hierarchy
          @_view_hierarchy || self.class.view_hierarchy
        end

        # Set the view hierarchy to use for the remainder of the request.
        # This affects all subsequent view/render calls in the request,
        # including templates rendered inside other templates.
        def set_view_hierarchy(dirs)
          @_view_hierarchy = dirs.map{|dir| dir.freeze}.freeze
        end

        private

        if Render::COMPILED_METHOD_SUPPORT
          # Include the view hierarchy in the template method cache key, so
          # that requests using different hierarchies use different compiled
          # template methods.
          def _cached_template_method_key(template)
            if hierarchy = view_hierarchy
              template = [hierarchy, template].freeze
            end

            super
          end

          # Include the view hierarchy when looking up compiled template
          # methods, matching _cached_template_method_key.
          def _cached_template_method_lookup(method_cache, template)
            if hierarchy = view_hierarchy
              template = [hierarchy, template]
            end

            super
          end
        end

        # If a view hierarchy is in effect, check each hierarchy directory
        # in order, returning the path of the first template file that
        # exists.  If no hierarchy directory contains the template, use the
        # default template path.
        def template_path(opts)
          if hierarchy = (opts[:view_hierarchy] || view_hierarchy)
            hierarchy.each do |dir|
              dir = self.class.expand_path(dir, render_opts[:views])
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
