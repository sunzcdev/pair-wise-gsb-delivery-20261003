# frozen-string-literal: true

#
class Roda
  module RodaPlugins
    # The autoload_named_routes plugin builds on the named_routes plugin and allows for
    # delaying loading of a file containing a named route for an application until there
    # is a request that uses the named route. This can be useful in development
    # to improvement startup time by not loading all named routes up front.  It can also be
    # useful in testing subsets of an application by only loading the named routes being
    # tested.
    #
    # You can specify a single hash branch for autoloading:
    #
    #   plugin :autoload_named_route
    #   autoload_named_route(:route_name, '/absolute/path/to/file')
    #   autoload_named_route(:namespace, :route_name, 'relative/path/to/file')
    #
    # Note that unlike the +route+ method defined by the named_routes plugin, when providing
    # a namespace, the namespace comes before the route name and not after.
    #
    # When the autoloaded file is required, it should redefine the same
    # named route.  If it does not, requests to the named route will be ignored (as if the
    # related named route block was empty).
    #
    # Loading of autoloaded named routes is thread-safe.  If multiple threads
    # make requests for the same autoloaded named route concurrently, the
    # related file is only loaded once, and all of the threads will dispatch
    # to the loaded named route.
    #
    # The loading state is tracked separately for each class, so a subclass
    # and the class it inherited from do not affect each other's loading
    # state.  This ensures that loading a named route file only publishes
    # the loaded routes to the application class (and namespace) that the
    # file itself configures.
    #
    # If loading the file for an autoloaded named route raises an error, the
    # error is recorded.  Any named routes that the file defined before
    # raising the error are replaced with routes that reraise the recorded
    # error, so that a partially loaded file does not result in some of its
    # routes handling requests.  Later requests for any named route related
    # to the file reraise the recorded error instead of loading the file
    # again.
    #
    # When freezing an application, all named routes are automatically loaded, because
    # autoloading named routes does not work for frozen applications.
    module AutoloadNamedRoutes
      def self.load_dependencies(app)
        app.plugin :named_routes
      end

      def self.configure(app)
        app.opts[:autoload_named_route_files] ||= []
        app.opts[:autoload_named_route_load_states] ||= {}
        app.opts[:autoload_named_route_mutex] ||= Mutex.new
      end

      module ClassMethods
        RodaPlugins.opt_attr_reader(self, :autoload_named_route_files)

        # Autoload the given file when there is request for the named route.
        # The given file should configure the named route specified.
        def autoload_named_route(namespace=nil, name, file)
          file = File.expand_path(file)
          autoload_named_route_files << file
          routes = namespaced_routes[namespace] ||= {}
          meth = routes[name] = define_roda_method(routes[name] || "named_routes_#{namespace}_#{name}", 1) do |r|
            loc = method(routes[name]).source_location
            self.class.load_autoload_named_route(file)
            # Avoid infinite loop in case method is not overridden
            if method(meth).source_location != loc
              send(meth, r)
            end
          end
          nil
        end

        # Load the file for an autoloaded named route, unless the file has
        # already been loaded by the current class.  The file is only loaded
        # a single time, even if this method is called concurrently in
        # multiple threads.
        #
        # If a previous attempt to load the file raised an error, this
        # reraises the recorded error instead of loading the file again.
        def load_autoload_named_route(file)
          states = opts[:autoload_named_route_load_states]

          # The loading states are removed when the class is frozen.  If
          # they are not present, the file should have already been loaded
          # when the class was frozen, so this require should be a no-op.
          return require file unless states

          opts[:autoload_named_route_mutex].synchronize do
            case state = states[file]
            when nil
              begin
                require file
              rescue Exception => e
                states[file] = e
                _handle_failed_autoload_named_route(file, e)
                raise
              end
              states[file] = true
            when true
              # File already loaded, nothing to do
            else
              # Previous attempt to load the file raised an error, reraise it
              raise state
            end
          end

          nil
        end

        # Eagerly load all autoloaded named routes when freezing the application.
        def freeze
          unless opts.frozen?
            autoload_named_route_files.each{|file| load_autoload_named_route(file)}
            opts.delete(:autoload_named_route_files)
            opts.delete(:autoload_named_route_load_states)
            opts.delete(:autoload_named_route_mutex)
          end
          super
        end

        private

        # Handle a failure to load the given file by replacing any named
        # routes the file defined with routes that reraise the recorded
        # error.  This ensures that a file that defined some routes before
        # raising an error does not result in those partially registered
        # routes handling requests.
        def _handle_failed_autoload_named_route(file, error)
          files = [file, "#{file}.rb"]
          namespaced_routes.each_value do |routes|
            routes.each do |name, meth|
              loc = instance_method(meth).source_location
              if loc && files.any?{|f| File.identical?(f, loc[0])}
                routes[name] = define_roda_method(meth, 1){|_| raise error}
              end
            end
          end
          nil
        end
      end
    end

    register_plugin(:autoload_named_routes, AutoloadNamedRoutes)
  end
end
