# frozen-string-literal: true

# RODA4: Remove plugin

#
class Roda
  module RodaPlugins
    # The route_block_args plugin lets you customize what arguments are passed to
    # the +route+ block.  So if you have an application that always needs access
    # to the +response+, the +params+, the +env+, or the +session+, you can use
    # this plugin so that any of those can be arguments to the route block.
    # Example:
    #
    #   class App < Roda
    #     plugin :route_block_args do
    #       [request, request.params, response]
    #     end
    #
    #     route do |r, params, res|
    #       r.post do
    #         artist = Artist.create(name: params['name'].to_s)
    #         res.status = 201
    #         artist.id.to_s
    #       end
    #     end
    #   end
    #
    # == Namespace Specific Arguments
    #
    # In larger applications, different parts of the routing tree may need
    # different arguments.  You can register arguments for a specific namespace
    # using the +route_block_args+ class method, or by loading the plugin with
    # the namespace:
    #
    #   class App < Roda
    #     plugin :route_block_args do
    #       [request, response]
    #     end
    #
    #     route_block_args :admin do
    #       [request, current_user, response]
    #     end
    #
    #     route('dashboard', 'admin') do |r, user, res|
    #       # uses the arguments registered for the admin namespace
    #     end
    #   end
    #
    # You can also load the plugin with a namespace to only register arguments
    # for that namespace:
    #
    #   plugin :route_block_args, :admin do
    #     [request, current_user, response]
    #   end
    #
    # Named routes, hash branches, and hash paths use the arguments registered
    # for their namespace.  Route blocks whose namespace does not have registered
    # arguments, as well as the main route block, use the arguments given to the
    # plugin (or just the request, if the plugin was loaded only with namespaces).
    #
    # Arguments are computed lazily, only when a route block that uses them is
    # called.  They are computed at most once per request for each argument
    # source.  So an expensive argument source (such as a database query) is not
    # run for branches that are not reached, is run at most once per request if
    # multiple route blocks sharing the same source are called, and is never
    # shared with route blocks using a different namespace's argument source.
    #
    # This plugin will be no longer ship with Roda starting in Roda 4. Users
    # are encouraged to switch to defining local variables at the top of the
    # route block instead of using this plugin.
    module RouteBlockArgs
      def self.configure(app, namespace=nil, &block)
        app.instance_exec do
          opts[:route_block_args_sources] ||= {}
          if namespace
            route_block_args(namespace, &block)
          else
            route_block_args(&block)
            route(&@raw_route_block) if @raw_route_block
          end
        end
      end

      module ClassMethods
        RodaPlugins.opt_attr_reader(self, :route_block_args_sources)

        # Freeze the registered namespace arguments when freezing the app.
        def freeze
          route_block_args_sources.freeze
          super
        end

        # Register the arguments to use for route blocks with the given namespace.
        # If no namespace is given, register the default arguments used by route
        # blocks that do not use a namespace with registered arguments.
        def route_block_args(namespace=nil, &block)
          raise RodaError, "route_block_args requires a block" unless block

          if namespace
            route_block_args_sources[namespace] = define_roda_method("route_block_args", 0, &block)
          else
            define_roda_method(:_roda_route_block_args, 0, &block)
          end

          nil
        end

        private

        # Override the route block input so that the block
        # given is passed the arguments specified by the
        # block given to the route_block_args plugin, or the
        # arguments registered for the route's namespace.
        def convert_route_block(block)
          namespace = converting_route_block_namespace
          meth = define_roda_method("convert_route_block_args", :any, &super(block))
          lambda do |_|
            send(meth, *_roda_route_block_args_for(namespace))
          end
        end
      end

      module InstanceMethods
        private

        # Default route block arguments, used when no block was given when
        # loading the plugin, and no arguments are registered for the route's
        # namespace.
        def _roda_route_block_args
          request
        end

        # Return the arguments for the given route namespace, computing them at
        # most once per request for each argument source.
        def _roda_route_block_args_for(namespace)
          meth = self.class.route_block_args_sources[namespace]
          cache = (@_roda_route_block_args_cache ||= {})

          # Use the argument source method itself as the cache key, and use a
          # separate key (nil) for the default arguments.  This ensures route
          # blocks with their own argument source never use arguments computed
          # for a different source, while route blocks that fall back to the
          # default arguments share a single computation.
          if cache.key?(meth)
            cache[meth]
          else
            cache[meth] = meth ? send(meth) : _roda_route_block_args
          end
        end
      end
    end

    register_plugin :route_block_args, RouteBlockArgs
  end
end
