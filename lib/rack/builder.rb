# frozen_string_literal: true

require_relative 'urlmap'

module Rack; end
Rack::BUILDER_TOPLEVEL_BINDING = ->(builder){builder.instance_eval{binding}}

module Rack
  # Rack::Builder provides a domain-specific language (DSL) to construct Rack
  # applications. It is primarily used to parse +config.ru+ files which
  # instantiate several middleware and a final application which are hosted
  # by a Rack-compatible web server.
  #
  # Example:
  #
  #   app = Rack::Builder.new do
  #     use Rack::CommonLogger
  #     map "/ok" do
  #       run lambda { |env| [200, {'content-type' => 'text/plain'}, ['OK']] }
  #     end
  #   end
  #
  #   run app
  #
  # Or
  #
  #   app = Rack::Builder.app do
  #     use Rack::CommonLogger
  #     run lambda { |env| [200, {'content-type' => 'text/plain'}, ['OK']] }
  #   end
  #
  #   run app
  #
  # +use+ adds middleware to the stack, +run+ dispatches to an application.
  # You can use +map+ to construct a Rack::URLMap in a convenient way.
  #
  # A Builder can also produce an explicit build plan via #to_plan.  The
  # plan captures the declared structure (middleware classes, initialization
  # arguments, +map+ hierarchy, +run+ endpoint, +warmup+ and +freeze_app+
  # directives) as inspectable data, without instantiating middleware or
  # calling the application to guess its structure.  The same plan can be
  # materialized into a fresh application instance whenever needed, which
  # allows preparing a new instance and only publishing it once it was
  # built and warmed up successfully.
  class Builder

    # https://stackoverflow.com/questions/2223882/whats-the-difference-between-utf-8-and-utf-8-without-bom
    UTF_8_BOM = '\xef\xbb\xbf'

    # Marker used in Plan#to_h output for components that contain arbitrary
    # Ruby code (blocks and other callable objects) which cannot be
    # statically reviewed.
    OPAQUE = :opaque

    # An explicit, reviewable description of how a Rack application should
    # be assembled from a Rack::Builder definition.
    #
    # A plan preserves the hierarchy of the builder it was captured from:
    # the +run+ endpoint, the ordered layers (Use and Map), the +warmup+
    # hook, the +freeze_app+ flag and the builder options.  Middleware are
    # recorded by identity (the class/object passed to +use+) together with
    # their initialization arguments, so the structure can be reviewed with
    # #to_h without instantiating anything or calling the application.
    #
    # Components that embed arbitrary Ruby code (a +run+ block, a block
    # given to +use+, or a +warmup+ hook) cannot be statically interpreted
    # and are reported as OPAQUE.  #opaque? returns true if the plan
    # contains any such component.  A plan captured from an untrusted rackup
    # file is NOT safe to execute: evaluating the rackup file already runs
    # its code, and Plan#to_app will run whatever the builder recorded.
    # Review plans with #to_h/#opaque? instead of assuming safety.
    #
    # Plan#to_app materializes a completely fresh application instance on
    # every call: middleware and URLMap instances are never shared between
    # builds, so repeated builds cannot leak instances from a previous
    # build into the next stack.  This supports a prepare-then-publish
    # workflow:
    #
    #   plan = builder.to_plan
    #   begin
    #     new_app = plan.to_app # builds and warms up the new instance
    #   rescue => error
    #     # the previously published application is untouched and keeps
    #     # working; resources created by the failed build were closed
    #   else
    #     # publish new_app (swap it in atomically at your convenience)
    #   end
    #
    # If building or warming up fails, Plan#to_app closes every resource it
    # created during that build (instances responding to #close, in reverse
    # order of creation) before re-raising.  Applications supplied from the
    # outside (the +run+ endpoint, default applications) are not owned by
    # the plan and are never closed.
    class Plan
      # A single +use+ statement: a middleware and its initialization
      # arguments.
      class Use
        attr_reader :middleware, :arguments, :keywords, :block

        def initialize(middleware, arguments, keywords, block)
          @middleware = middleware
          @arguments = arguments
          @keywords = keywords
          @block = block
        end

        # Instantiate the middleware around +app+, registering the new
        # instance as an owned resource of the current build.
        def build(app, owned)
          instance = @middleware.new(app, *@arguments, **@keywords, &@block)
          owned << instance
          instance
        end

        # Whether this layer contains arbitrary Ruby code (a block given to
        # +use+) that cannot be statically reviewed.
        def opaque?
          !@block.nil?
        end

        def to_h
          {
            type: :use,
            middleware: @middleware,
            arguments: @arguments,
            keywords: @keywords,
            block: @block ? OPAQUE : nil,
          }
        end
      end

      # A single +map+ statement: a set of paths, each mapped to a nested
      # Plan.
      class Map
        # A hash of path => Plan.
        attr_reader :mapping

        def initialize(mapping)
          @mapping = mapping
        end

        # Build a URLMap for the nested plans.  +default_app+ is the
        # application accumulated by the outer plan so far; it is used for
        # the root path and as the default endpoint of nested plans that do
        # not declare their own +run+.
        def build(default_app, owned)
          mapped = default_app ? { '/' => default_app } : {}
          @mapping.each { |path, plan| mapped[path] = plan.build(owned, default_app) }
          urlmap = URLMap.new(mapped)
          owned << urlmap
          urlmap
        end

        def opaque?
          @mapping.each_value.any?(&:opaque?)
        end

        def to_h
          {
            type: :map,
            mapping: @mapping.transform_values(&:to_h),
          }
        end
      end

      # A +map+ statement that has been declared but not yet compiled into
      # the plan.  Holds the raw path => block mapping.
      # :nodoc:
      class PendingMap
        attr_reader :mapping

        def initialize(mapping)
          @mapping = mapping
        end
      end

      # The endpoint given to +run+, or nil if the plan relies on a default
      # application (only valid for nested plans inside a Map layer).
      attr_reader :run

      # The ordered layers of the plan, innermost first.  Each element is a
      # Use or Map instance.
      attr_reader :layers

      # Any options the builder was initialized with.
      attr_reader :options

      def initialize(run, layers, warmup, freeze_app, options)
        @run = run
        @layers = layers
        @warmup = warmup
        @freeze_app = freeze_app
        @options = options
      end

      # Whether the application and middleware instances will be frozen
      # when the plan is materialized.
      def freeze_app?
        @freeze_app
      end

      # Whether any component of this plan contains arbitrary Ruby code
      # that cannot be statically reviewed (a +run+ block, a block given to
      # +use+, a +warmup+ hook, or any opaque component of a nested plan).
      def opaque?
        @run.is_a?(Proc) || !@warmup.nil? || @layers.any?(&:opaque?)
      end

      # Whether every component of this plan is statically reviewable.
      def reviewable?
        !opaque?
      end

      # A reviewable, nested-hash description of the plan.  Middleware are
      # represented by their class/object and initialization arguments;
      # components containing arbitrary Ruby code are represented by
      # OPAQUE.
      def to_h
        {
          run: @run.is_a?(Proc) ? OPAQUE : @run,
          layers: @layers.map(&:to_h),
          warmup: @warmup ? OPAQUE : nil,
          freeze_app: @freeze_app,
          options: @options,
        }
      end

      # Materialize a fresh Rack application instance from this plan.
      #
      # Every call builds a completely new stack: middleware and URLMap
      # instances are created anew and never carried over from a previous
      # build.  If instantiation or warmup fails, all resources created by
      # this build (instances responding to #close) are closed in reverse
      # order of creation, and the error is re-raised, so a previously
      # built/published application remains fully usable.
      def to_app
        owned = []
        begin
          build(owned)
        rescue Exception
          close_owned(owned)
          raise
        end
      end

      # Build the application, appending every resource created along the
      # way to +owned+.  +default_app+ is used as the endpoint if this plan
      # does not declare its own +run+ (nested plans inside a Map layer).
      # This is an internal method used by Map layers; use #to_app instead.
      # :nodoc:
      def build(owned, default_app = nil)
        app = @run || default_app
        fail "missing run or map statement" unless app || @layers.first.is_a?(Map)

        # Freeze the endpoint itself, unless it is about to be wrapped in a
        # URLMap (historically only the URLMap is frozen in that case).
        app.freeze if @freeze_app && app && !@layers.first.is_a?(Map)

        @layers.each do |layer|
          app = layer.build(app, owned)
          app.freeze if @freeze_app
        end

        @warmup.call(app) if @warmup
        app
      end

      private

      def close_owned(owned)
        owned.reverse_each do |resource|
          begin
            resource.close if resource.respond_to?(:close)
          rescue Exception
            # Ignore cleanup failures so the original error propagates.
          end
        end
      end
    end

    # Parse the given config file to get a Rack application.
    #
    # If the config file ends in +.ru+, it is treated as a
    # rackup file and the contents will be treated as if
    # specified inside a Rack::Builder block.
    #
    # If the config file does not end in +.ru+, it is
    # required and Rack will use the basename of the file
    # to guess which constant will be the Rack application to run.
    #
    # Examples:
    #
    #   Rack::Builder.parse_file('config.ru')
    #   # Rack application built using Rack::Builder.new
    #
    #   Rack::Builder.parse_file('app.rb')
    #   # requires app.rb, which can be anywhere in Ruby's
    #   # load path. After requiring, assumes App constant
    #   # is a Rack application
    #
    #   Rack::Builder.parse_file('./my_app.rb')
    #   # requires ./my_app.rb, which should be in the
    #   # process's current directory.  After requiring,
    #   # assumes MyApp constant is a Rack application
    def self.parse_file(path, **options)
      if path.end_with?('.ru')
        return self.load_file(path, **options)
      else
        require path
        return Object.const_get(::File.basename(path, '.rb').split('_').map(&:capitalize).join(''))
      end
    end

    # Load the given file as a rackup file, treating the
    # contents as if specified inside a Rack::Builder block.
    #
    # Ignores content in the file after +__END__+, so that
    # use of +__END__+ will not result in a syntax error.
    #
    # Example config.ru file:
    #
    #   $ cat config.ru
    #
    #   use Rack::ContentLength
    #   require './app.rb'
    #   run App
    def self.load_file(path, **options)
      config = ::File.read(path)
      config.slice!(/\A#{UTF_8_BOM}/) if config.encoding == Encoding::UTF_8

      if config[/^#\\(.*)/]
        fail "Parsing options from the first comment line is no longer supported: #{path}"
      end

      config.sub!(/^__END__\n.*\Z/m, '')

      return new_from_string(config, path, **options)
    end

    # Evaluate the given +builder_script+ string in the context of
    # a Rack::Builder block, returning a Rack application.
    def self.new_from_string(builder_script, path = "(rackup)", **options)
      builder = self.new(**options)

      # We want to build a variant of TOPLEVEL_BINDING with self as a Rack::Builder instance.
      # We cannot use instance_eval(String) as that would resolve constants differently.
      binding = BUILDER_TOPLEVEL_BINDING.call(builder)
      eval(builder_script, binding, path)

      return builder.to_app
    end

    # Initialize a new Rack::Builder instance.  +default_app+ specifies the
    # default application if +run+ is not called later.  If a block
    # is given, it is evaluated in the context of the instance.
    def initialize(default_app = nil, **options, &block)
      @use = []
      @map = nil
      @run = default_app
      @warmup = nil
      @freeze_app = false
      @options = options

      instance_eval(&block) if block_given?
    end

    # Any options provided to the Rack::Builder instance at initialization.
    # These options can be server-specific. Some general options are:
    #
    # * +:isolation+: One of +process+, +thread+ or +fiber+. The execution
    #   isolation model to use.
    attr :options

    # Create a new Rack::Builder instance and return the Rack application
    # generated from it.
    def self.app(default_app = nil, &block)
      self.new(default_app, &block).to_app
    end

    # Specifies middleware to use in a stack.
    #
    #   class Middleware
    #     def initialize(app)
    #       @app = app
    #     end
    #
    #     def call(env)
    #       env["rack.some_header"] = "setting an example"
    #       @app.call(env)
    #     end
    #   end
    #
    #   use Middleware
    #   run lambda { |env| [200, { "content-type" => "text/plain" }, ["OK"]] }
    #
    # All requests through to this application will first be processed by the middleware class.
    # The +call+ method in this example sets an additional environment key which then can be
    # referenced in the application if required.
    def use(middleware, *args, **kwargs, &block)
      if @map
        mapping, @map = @map, nil
        @use << Plan::PendingMap.new(mapping)
      end
      @use << Plan::Use.new(middleware, args, kwargs, block)

      nil
    end

    # Takes a block or argument that is an object that responds to #call and
    # returns a Rack response.
    #
    # You can use a block:
    #
    #   run do |env|
    #     [200, { "content-type" => "text/plain" }, ["Hello World!"]]
    #   end
    #
    # You can also provide a lambda:
    #
    #   run lambda { |env| [200, { "content-type" => "text/plain" }, ["OK"]] }
    #
    # You can also provide a class instance:
    #
    #   class Heartbeat
    #     def call(env)
    #      [200, { "content-type" => "text/plain" }, ["OK"]]
    #     end
    #   end
    #
    #   run Heartbeat.new
    #
    def run(app = nil, &block)
      raise ArgumentError, "Both app and block given!" if app && block_given?

      @run = app || block

      nil
    end

    # Takes a lambda or block that is used to warm-up the application. This block is called
    # before the Rack application is returned by to_app.
    #
    #   warmup do |app|
    #     client = Rack::MockRequest.new(app)
    #     client.get('/')
    #   end
    #
    #   use SomeMiddleware
    #   run MyApp
    def warmup(prc = nil, &block)
      @warmup = prc || block
    end

    # Creates a route within the application.  Routes under the mapped path will be sent to
    # the Rack application specified by run inside the block.  Other requests will be sent to the
    # default application specified by run outside the block.
    #
    #   class App
    #     def call(env)
    #       [200, {'content-type' => 'text/plain'}, ["Hello World"]]
    #     end
    #   end
    #
    #   class Heartbeat
    #     def call(env)
    #       [200, { "content-type" => "text/plain" }, ["OK"]]
    #     end
    #   end
    #
    #   app = Rack::Builder.app do
    #     map '/heartbeat' do
    #       run Heartbeat.new
    #     end
    #     run App.new
    #   end
    #
    #   run app
    #
    # The +use+ method can also be used inside the block to specify middleware to run under a specific path:
    #
    #   app = Rack::Builder.app do
    #     map '/heartbeat' do
    #       use Middleware
    #       run Heartbeat.new
    #     end
    #     run App.new
    #   end
    #
    # This example includes a piece of middleware which will run before +/heartbeat+ requests hit +Heartbeat+.
    #
    # Note that providing a +path+ of +/+ will ignore any default application given in a +run+ statement
    # outside the block.
    def map(path, &block)
      @map ||= {}
      @map[path] = block

      nil
    end

    # Freeze the app (set using run) and all middleware instances when building the application
    # in to_app.
    def freeze_app
      @freeze_app = true
    end

    # Return an explicit build plan for this builder.  The plan captures
    # the declared structure (the +run+ endpoint, the ordered +use+ and
    # +map+ layers with their middleware classes/objects and initialization
    # arguments, the +warmup+ hook, the +freeze_app+ flag and the builder
    # options) as reviewable data, without instantiating middleware or
    # calling the application to guess its structure.
    #
    # Note that capturing a plan evaluates the blocks given to +map+ (just
    # like #to_app does), so a plan does not make an untrusted rackup file
    # safe to load; components containing arbitrary Ruby code are marked as
    # opaque in the plan (see Plan#opaque? and Plan#to_h).
    #
    # The returned plan can be materialized any number of times; each
    # Plan#to_app call builds a completely fresh application instance, so
    # a new instance can be prepared (and warmed up) before it is
    # published, without disturbing the previously built one.
    def to_plan
      layers = []
      layers << compile_map(@map) if @map
      @use.reverse_each do |entry|
        layers << (entry.is_a?(Plan::PendingMap) ? compile_map(entry.mapping) : entry)
      end
      Plan.new(@run, layers, @warmup, @freeze_app, @options)
    end

    # Return the Rack application generated by this instance.
    #
    # This is equivalent to <tt>to_plan.to_app</tt>: it materializes a
    # fresh application instance from the builder's current plan.  See
    # Plan#to_app for the failure and cleanup semantics.
    def to_app
      to_plan.to_app
    end

    # Call the Rack application generated by this builder instance. Note that
    # this rebuilds the Rack application and runs the warmup code (if any)
    # every time it is called, so it should not be used if performance is important.
    def call(env)
      to_app.call(env)
    end

    private

    # Compile a path => block mapping into a Plan::Map layer whose values
    # are nested plans, one per mapped block.
    def compile_map(mapping)
      Plan::Map.new(mapping.transform_values { |block| self.class.new(&block).to_plan })
    end
  end
end
