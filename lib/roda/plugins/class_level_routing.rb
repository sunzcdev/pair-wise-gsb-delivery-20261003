# frozen-string-literal: true

#
class Roda
  module RodaPlugins
    # The class_level_routing plugin adds routing methods at the class level, which can
    # be used instead of or in addition to using the normal +route+ method to start the
    # routing tree.  If a request is not matched by the normal routing tree, the class
    # level routes will be tried.  This offers a more Sinatra-like API, while
    # still allowing you to use a routing tree inside individual actions.
    #
    # Here's the first example from the README, modified to use the class_level_routing
    # plugin:
    #
    #   class App < Roda
    #     plugin :class_level_routing
    #
    #     # GET / request
    #     root do
    #       request.redirect "/hello"
    #     end
    #
    #     # GET /hello/world request
    #     get "hello/world" do
    #       "Hello world!"
    #     end
    #
    #     # /hello request
    #     is "hello" do
    #       # Set variable for both GET and POST requests
    #       @greeting = 'Hello'
    #
    #       # GET /hello request
    #       request.get do
    #         "#{@greeting}!"
    #       end
    #
    #       # POST /hello request
    #       request.post do
    #         puts "Someone said #{@greeting}!"
    #         request.redirect
    #       end
    #     end
    #   end
    #
    # When using the class_level_routing plugin with nested routes, you may also want to use the
    # delegate plugin to delegate certain instance methods to the request object, so you don't have
    # to continually use +request.+ in your routing blocks, or use the r plugin so you can use +r+
    # instead of +request+.
    #
    # Note that class level routing is implemented via a simple array of routes, so routing performance
    # will degrade linearly as the number of routes increases.  For best performance, you should use
    # the normal +route+ class method to define your routing tree.  This plugin does make it simpler to
    # add additional routes after the routing tree has already been defined, though.
    #
    # === Named Routes and Removing Routes in Subclasses
    #
    # Each class level route can be given an explicit identity when it is declared, by
    # passing an +:name+ option.  The name can be any symbol (or string), and uniquely
    # identifies the route within the application:
    #
    #   class App < Roda
    #     plugin :class_level_routing
    #
    #     get "products", name: :products_index do
    #       "all products"
    #     end
    #   end
    #
    # Subclasses (e.g. the per-product gateways derived from a shared parent application)
    # can then remove an individual inherited route with remove_class_level_route, while
    # leaving every other method on the same path, and every other subclass, untouched:
    #
    #   App.freeze
    #
    #   class GatewayA < App
    #     remove_class_level_route(:products_index)
    #   end
    #
    #   class GatewayB < App
    #   end
    #
    #   # GatewayA no longer serves the named route...
    #   GatewayA.call(Request.new("GET", "/products"))[0] # => 404
    #   # ...while the sibling subclass and the parent still do
    #   GatewayB.call(Request.new("GET", "/products"))[0] # => 200
    #   App.call(Request.new("GET", "/products"))[0]      # => 200
    #
    # Routes without an explicit +:name+ are still assigned a stable identity based on
    # the request method and the match arguments, so they can be removed by describing
    # the route instead of naming it:
    #
    #   get "legacy" do
    #     "legacy"
    #   end
    #
    #   class Subclass < App
    #     remove_class_level_route(:get, "legacy")
    #   end
    #
    # Removing a route only takes that single entry out of the candidate list for the
    # subclass.  Any other class level routes that could still match the same path
    # (a different request method, or a broader +on+/+is+ branch) remain in place and
    # continue to be tried.  If no route with the given identity can be found,
    # remove_class_level_route raises a RodaError so the mismatch is reported immediately
    # rather than silently leaving the route in place.
    #
    # Because each subclass keeps its own copy of the class level routes, a parent
    # application may be frozen before the subclasses are derived; removing a route in a
    # subclass never mutates the frozen parent's routes.
    module ClassLevelRouting
      SCOPE_INSTANCE_VARIABLES = [:@_original_remaining_path].freeze

      # Prepended so the inherited hook runs before Roda's own Roda.inherited.  Roda
      # normally refuses to subclass a frozen application (other plugins keep mutable
      # arrays/hashes that the core hook knows how to duplicate), but a gateway
      # architecture wants to freeze the shared parent and then derive subclasses from
      # it.  This plugin's state is safe to duplicate from a frozen parent, so when the
      # parent is frozen this performs the standard setup itself, without mutating the
      # parent; unfrozen parents keep using Roda's standard inheritance.
      module InheritedClassMethods
        # Allow a frozen parent application using class level routing to be subclassed,
        # and give the subclass its own mutable copy of the class level routes.
        def inherited(subclass)
          unless frozen?
            super
            return
          end

          # Frozen parent: Roda.inherited would raise, so replicate its setup while
          # only reading from the parent, leaving the frozen parent untouched.
          subclass.instance_variable_set(:@inherit_middleware, @inherit_middleware)
          subclass.instance_variable_set(:@middleware, @inherit_middleware ? @middleware.dup : [])
          subclass.instance_variable_set(:@plugins, @plugins.dup)

          opts = RodaOpts.new
          opts.roda_class = subclass
          @opts.each do |k, v|
            opts[k] = if k == :subclassed
              false
            elsif k == :class_level_routes
              # Always give the subclass an independent, mutable copy, even though the
              # parent's array is frozen and would otherwise be shared.
              v.dup
            else
              v
            end
          end
          subclass.instance_variable_set(:@opts, opts)
          subclass.instance_variable_set(:@opt_class_level_routes, opts[:class_level_routes])

          if block = @raw_route_block
            subclass.route(&block)
          end

          request_class = Class.new(self::RodaRequest)
          request_class.roda_class = subclass
          request_class.match_pattern_cache = RodaCache.new
          subclass.const_set(:RodaRequest, request_class)

          response_class = Class.new(self::RodaResponse)
          response_class.roda_class = subclass
          subclass.const_set(:RodaResponse, response_class)
        end
      end

      module ClassMethods
        RodaPlugins.opt_attr_reader(self, :class_level_routes)

        # Define routing methods that will store class level routes.  A route may be
        # given an explicit +:name+ so it can be precisely referenced later with
        # remove_class_level_route.
        %i[root on is get post delete head options link patch put trace unlink query].each do |request_meth|
          define_method(request_meth) do |*args, name: nil, &block|
            meth = define_roda_method("class_level_routing_#{request_meth}", :any, &block)
            class_level_routes << {
              request_method: request_meth,
              args: args.freeze,
              method: meth,
              name: name
            }.freeze
          end
        end

        # Remove a previously declared class level route so that the current class no
        # longer serves it, without affecting the parent class or any sibling subclass.
        #
        # The route to remove can be identified in two ways:
        #
        # * The explicit +:name+ given when the route was declared:
        #
        #     get "products", name: :products_index do
        #       "all products"
        #     end
        #
        #     remove_class_level_route(:products_index)
        #
        # * The request method symbol followed by the match arguments, which identify a
        #   route that was declared without an explicit name:
        #
        #     get "legacy" do
        #       "legacy"
        #     end
        #
        #     remove_class_level_route(:get, "legacy")
        #
        # Only the matching entry is removed.  Other class level routes that can still
        # match the same request (for example, another request method on the same path,
        # or a surrounding +on+ branch) are retained and remain candidates.
        #
        # Raises RodaError if no class level route with the given identity exists, so
        # that an incorrect reference is reported instead of being silently ignored.
        def remove_class_level_route(name, *args)
          route = if args.empty?
            class_level_routes.find do |r|
              r[:name] && r[:name].to_s == name.to_s
            end
          else
            request_meth = name
            class_level_routes.find do |r|
              r[:request_method] == request_meth && r[:args] == args
            end
          end

          unless route
            if args.empty?
              raise RodaError, "cannot remove class level route, no route named #{name.inspect}"
            else
              raise RodaError, "cannot remove class level route, no #{name.inspect} route with args #{args.inspect}"
            end
          end

          class_level_routes.delete(route)

          # Remove the generated handler method as well, but only when it was defined
          # directly on this class and no remaining route references it.  Handler
          # methods of inherited routes live on the parent and must stay there for the
          # parent and any sibling subclasses; deleting the route entry above already
          # stops this subclass from dispatching to it.  Method names are generated
          # sequentially, so separate route declarations normally get distinct methods,
          # but the remaining-route check keeps the cleanup safe if two entries share
          # one.
          handler = route[:method]
          if private_method_defined?(handler, false) &&
             class_level_routes.none? { |r| r[:method] == handler }
            remove_method(handler)
          end

          route
        end

        # Freeze the class level routes so that there can be no thread safety issues at runtime.
        def freeze
          class_level_routes.freeze
          super
        end
      end

      # Prepend the inheritance hook, then extend the regular class methods.
      def self.configure(app)
        app.opts[:class_level_routes] ||= []
        app.singleton_class.send(:prepend, InheritedClassMethods) unless app.singleton_class.ancestors.include?(InheritedClassMethods)
      end

      module InstanceMethods
        def initialize(_)
          super
          @_original_remaining_path = @_request.remaining_path
        end

        private

        # If the normal routing tree doesn't handle an action, try each class level route
        # to see if it matches.
        def _roda_after_10__class_level_routing(result)
          if result && result[0] == 404 && (v = result[2]).is_a?(Array) && v.empty?
            # Reset the response so it doesn't inherit the status or any headers from
            # the original response.
            @_response.send(:initialize)
            @_response.status = nil
            result.replace(_roda_handle_route do
              r = @_request
              self.class.class_level_routes.each do |route|
                r.instance_variable_set(:@remaining_path, @_original_remaining_path)
                r.public_send(route[:request_method], *route[:args]) do |*a|
                  send(route[:method], *a)
                end
              end
              nil
            end)
          end
        end
      end
    end

    register_plugin(:class_level_routing, ClassLevelRouting)
  end
end
