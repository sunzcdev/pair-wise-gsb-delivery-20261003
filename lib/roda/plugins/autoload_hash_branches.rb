# frozen-string-literal: true
require 'monitor'

#
class Roda
  module RodaPlugins
    # The autoload_hash_branches plugin builds on the hash_branches plugin and allows for
    # delaying loading of a file containing a hash branch for an application until there
    # is a request that uses the hash branch. This can be useful in development
    # to improvement startup time by not loading all branches up front.  It can also be
    # useful in testing subsets of an application by only loading the hash branches being
    # tested.
    #
    # You can specify a single hash branch for autoloading:
    #
    #   plugin :autoload_hash_branches
    #   autoload_hash_branch('branch_name', '/absolute/path/to/file')
    #   autoload_hash_branch('namespace', 'branch_name', 'relative/path/to/file')
    #
    # You can also set the plugin to autoload load all hash branch files in a given directory.
    # This will look at each .rb file in the directory, and add an autoload for it, using the
    # filename without the .rb as the branch name:
    #
    #   autoload_hash_branch_dir('/path/to/dir')
    #   autoload_hash_branch_dir('namespace', '/path/to/dir')
    #
    # In both cases, when the autoloaded file is required, it should redefine the same
    # hash branch.  If it does not, requests to the hash branch will result in a 404 error.
    #
    # Concurrent requests for a branch while it is being loaded wait for the single loading
    # request.  Loading one branch does not block requests for another branch.  If loading
    # raises an exception, all requests waiting for that branch receive the exception, the
    # partially registered routes are rolled back, and the next request can retry loading.
    # Recursively requesting the same branch from the loading thread raises a RodaError.
    #
    # When freezing an application, all hash branches are automatically loaded, because
    # autoloading hash branches does not work for frozen applications.
    module AutoloadHashBranches
      class LoadAttempt
        attr_accessor :status, :error
        attr_reader :state, :children

        def initialize(status=:loading, state=nil)
          @status = status
          @state = state
          @error = nil
          @owner = nil
          @children = []
        end

        def owner=(owner)
          @owner = owner
        end

        def owned_by?(thread)
          @owner == thread
        end

        def complete
          state.finish_attempt(self, :loaded)
        end

        def fail(error)
          children.reverse_each{|child| child.fail(error)}
          state.finish_attempt(self, :failed, error)
        end

        def initialize_copy(*)
          super
          @owner = nil
          @children = []
        end
      end

      class BranchLoader
        attr_reader :file
        attr_accessor :source_location

        def initialize(file)
          @file = file
          @mutex = Mutex.new
          @condition = ConditionVariable.new
          @attempt = LoadAttempt.new(:unloaded)
          @waiters = 0
        end

        def begin_load
          @mutex.synchronize do
            @condition.wait(@mutex) while @waiters > 0

            case @attempt.status
            when :loaded
              return :ready, nil
            when :loading
              raise RodaError, "recursively autoloading hash branch" if @attempt.owned_by?(Thread.current)

              @waiters += 1
              return :wait, @attempt
            end

            @attempt = LoadAttempt.new(:loading, self)
            @attempt.owner = Thread.current
            [:load, @attempt]
          end
        end

        def waiters
          @mutex.synchronize do
            @waiters
          end
        end

        def wait(attempt)
          @mutex.synchronize do
            @condition.wait(@mutex) while attempt.status == :loading
            @waiters -= 1
            @condition.broadcast
            raise attempt.error if attempt.status == :failed
          end
        end

        def finish_attempt(attempt, status, error=nil)
          @mutex.synchronize do
            attempt.status = status
            attempt.error = error
            @condition.broadcast
          end
        end

        def initialize_copy(*)
          super
          @mutex = Mutex.new
          @condition = ConditionVariable.new
          @waiters = 0
          @attempt = @attempt.dup
          @attempt.instance_variable_set(:@state, self)
        end
      end

      class RouteTransaction
        def initialize(manager)
          @manager = manager
          @snapshots = []
          @attempts = []
        end

        def attempt_started(attempt)
          @attempts << attempt
        end

        def attempt_finished
          @attempts.pop
        end

        def current_attempt
          @attempts.last
        end

        def remember(namespace, segment)
          @manager.synchronize_routes do
            routes = @manager.app.hash_branches[namespace]
            namespace_present = !routes.nil?
            meth = routes && routes[segment]
            method = if meth && @manager.app.private_method_defined?(meth)
              @manager.app.instance_method(meth)
            end

            @snapshots << [namespace, segment, namespace_present, meth, method]
          end
        end

        def snapshot_count
          @snapshots.length
        end

        def rollback_checkpoint(checkpoint)
          snapshots = @snapshots.slice!(checkpoint, @snapshots.length - checkpoint)
          rollback_snapshots(snapshots)
        end

        def rollback
          rollback_snapshots(@snapshots)
        end

        private

        def rollback_snapshots(snapshots)
          @manager.synchronize_routes do
            snapshots.reverse_each do |namespace, segment, namespace_present, meth, original_method|
              routes = @manager.app.hash_branches[namespace]

              if namespace_present
                if meth
                  new_meth = routes[segment]
                  routes[segment] = meth

                  if original_method
                    @manager.app.send(:define_method, meth, original_method)
                    @manager.app.send(:private, meth)
                    @manager.app.send(:alias_method, meth, meth)
                  elsif @manager.app.private_method_defined?(meth)
                    @manager.app.send(:remove_method, meth)
                  end

                  if new_meth && new_meth != meth && @manager.app.private_method_defined?(new_meth)
                    @manager.app.send(:remove_method, new_meth)
                  end
                elsif routes
                  new_meth = routes.delete(segment)
                  if new_meth && @manager.app.private_method_defined?(new_meth)
                    @manager.app.send(:remove_method, new_meth)
                  end
                end
              elsif @manager.app.hash_branches.delete(namespace) && meth && @manager.app.private_method_defined?(meth)
                @manager.app.send(:remove_method, meth)
              end
            end
          end
        end
      end

      class LoaderManager
        attr_reader :app, :loaders

        def initialize(app)
          @app = app
          @mutex = Mutex.new
          @route_mutex = Monitor.new
          @loaders = {}
          @transactions = {}
        end

        def loader(key)
          @loaders[key]
        end

        def set_loader(key, branch_loader)
          @loaders[key] = branch_loader
        end

        def for_subclass(app)
          manager = dup
          manager.instance_variable_set(:@app, app)
          manager
        end

        def synchronize_routes
          @route_mutex.synchronize{yield}
        end

        def current_route_transaction
          @mutex.synchronize{@transactions[Thread.current]}
        end

        def remember_route(transaction, namespace, segment)
          transaction.remember(namespace, segment) if transaction
        end

        def load(branch_loader)
          parent_transaction = current_route_transaction
          parent_attempt = parent_transaction && parent_transaction.current_attempt
          action, attempt = branch_loader.begin_load

          case action
          when :ready
            return :ready
          when :wait
            branch_loader.wait(attempt)
            return :ready
          end

          if parent_transaction
            transaction = parent_transaction
            checkpoint = transaction.snapshot_count
          else
            transaction = RouteTransaction.new(self)
            @mutex.synchronize{@transactions[Thread.current] = transaction}
          end

          transaction.attempt_started(attempt) if parent_transaction
          begin
            yield

            if parent_attempt
              transaction.attempt_finished
              parent_attempt.children << attempt
            else
              attempt.complete
            end
          rescue => error
            if parent_attempt
              transaction.attempt_finished
              parent_attempt.children.delete(attempt)
              transaction.rollback_checkpoint(checkpoint)
              attempt.fail(error)
            else
              transaction.rollback
              attempt.fail(error)
            end

            raise
          ensure
            unless parent_attempt
              @mutex.synchronize{@transactions.delete(Thread.current)}
            end
          end

          :dispatched
        end

        def initialize_copy(*)
          super
          @mutex = Mutex.new
          @route_mutex = Monitor.new
          @loaders = @loaders.dup
          @loaders.each{|key, branch_loader| @loaders[key] = branch_loader.dup}
          @transactions = {}
        end

      end

      def self.load_dependencies(app)
        app.plugin :hash_branches
      end

      def self.configure(app)
        app.opts[:autoload_hash_branch_files] ||= []
        app.opts[:autoload_hash_branch_keys] ||= []
        app.opts[:autoload_hash_branch_manager] ||= LoaderManager.new(app)
      end

      module ClassMethods
        RodaPlugins.opt_attr_reader(self, :autoload_hash_branch_files)
        RodaPlugins.opt_attr_reader(self, :autoload_hash_branch_keys)
        RodaPlugins.opt_attr_reader(self, :autoload_hash_branch_manager)

        # Autoload the given file when there is request for the hash branch.
        # The given file should configure the hash branch specified.
        def autoload_hash_branch(namespace='', segment, file)
          segment = "/#{segment}"
          file = File.expand_path(file)
          key = [namespace, segment].freeze

          autoload_hash_branch_files << file
          unless autoload_hash_branch_keys.include?(key)
            autoload_hash_branch_keys << key
          end

          autoload_hash_branch_manager.synchronize_routes do
            routes = hash_branches[namespace] ||= {}
            registered_segment = segment
            branch_loader = BranchLoader.new(file)
            autoload_hash_branch_manager.set_loader(key, branch_loader)
            meth = routes[registered_segment] = define_roda_method(routes[registered_segment] || "hash_branch_#{namespace}_#{segment}", 1) do |r|
              _roda_autoload_hash_branch_route(key, r)
            end
            branch_loader.source_location = instance_method(meth).source_location
          end

          nil
        end

        # For each .rb file in the given directory, add an autoloaded hash branch
        # based on the file name.
        def autoload_hash_branch_dir(namespace='', dir)
          Dir.new(dir).entries.each do |file|
            if file =~ /\.rb\z/i
              autoload_hash_branch(namespace, file.sub(/\.rb\z/i, ''), File.join(dir, file))
            end
          end
        end

        # Copy branch loading state into the subclass.
        def inherited(subclass)
          super
          subclass.opts[:autoload_hash_branch_manager] = autoload_hash_branch_manager.for_subclass(subclass)
        end

        def hash_branch(namespace='', segment, &block)
          registered_segment = segment.start_with?("/") ? segment : "/#{segment}"
          base_segment = registered_segment.sub(/\A\//, '')
          transaction = autoload_hash_branch_manager.current_route_transaction
          meth = autoload_hash_branch_manager.synchronize_routes do
            autoload_hash_branch_manager.remember_route(transaction, namespace, registered_segment)
            super(namespace, base_segment, &block)
          end
          meth
        end

        # Eagerly load all hash branches when freezing the application.
        def freeze
          unless opts.frozen?
            autoload_hash_branch_keys.each do |key|
              branch_loader = autoload_hash_branch_manager.loader(key)
              autoload_hash_branch_manager.load(branch_loader){require branch_loader.file}
            end

            opts[:autoload_hash_branch_files] = nil
            opts.delete(:autoload_hash_branch_files)
            opts[:autoload_hash_branch_keys] = nil
            opts.delete(:autoload_hash_branch_keys)
          end

          super
        end
      end

      module InstanceMethods
        private

        def _roda_autoload_hash_branch_route(key, request)
          manager = self.class.autoload_hash_branch_manager
          branch_loader = manager.loader(key)
          namespace, segment = key

          manager.load(branch_loader) do
            require branch_loader.file
          end

          meth = self.class.hash_branches[namespace][segment]
          if method(meth).source_location != branch_loader.source_location
            send(meth, request)
          end
        end
      end
    end

    register_plugin(:autoload_hash_branches, AutoloadHashBranches)
  end
end
