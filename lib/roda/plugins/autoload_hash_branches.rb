# frozen-string-literal: true

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
    # === Concurrency
    #
    # Loading a branch is single-flighted per branch:
    #
    # * If multiple concurrent requests hit an unloaded branch, only the first one loads
    #   the file.  The other requests wait for that load to finish and then dispatch to
    #   the fully loaded branch.
    # * Loading different branches is independent.  A request for one branch does not
    #   wait while another branch is being loaded.
    # * If loading the file raises an exception, every request waiting for the branch
    #   receives the exception.  Hash branches the file registered before raising are
    #   rolled back, so requests never use partially registered routes.  The file is not
    #   considered loaded, and a later request retries the load.
    # * The file being loaded must not request the same hash branch while it is being
    #   loaded.  Such recursive autoloading cannot be satisfied while the branch is still
    #   loading, and raises a RodaError.
    #
    # When freezing an application, all hash branches are automatically loaded, because
    # autoloading hash branches does not work for frozen applications.
    module AutoloadHashBranches
      # Per-branch single-flight state.  Each registered autoload hash branch gets its
      # own state, so loading one branch does not block loading other branches.
      class AutoloadState
        # Mutex protecting the state and the currently running loading attempt.
        attr_reader :mutex

        # The loading attempt currently in progress or most recently run.
        attr_accessor :attempt

        # The routing method symbol registered for the branch, used to reinstall the
        # loading method when rolling back a failed load.
        attr_accessor :meth

        # Namespace and segment identifying the branch in hash_branches.
        attr_accessor :namespace, :segment

        # Source location of the loading method, used to detect files that do not
        # redefine the branch.
        attr_accessor :loader_source_location

        def initialize
          @mutex = Mutex.new
        end
      end

      # A single attempt at loading a branch file.  A new attempt is created when a
      # request starts loading the branch, and the same attempt is shared by all
      # requests waiting for that load.  A failed attempt is never reused; later
      # requests create a new attempt and can retry the load.
      class AutoloadAttempt
        # The exception raised while loading the file, if the attempt failed.
        attr_reader :error

        def initialize
          @mutex = Mutex.new
          @condition = ConditionVariable.new
          @complete = false
          @error = nil
        end

        # Whether the loading attempt has finished, successfully or not.
        def complete?
          @mutex.synchronize { @complete }
        end

        # Mark the loading attempt as finished.  With no argument it is successful;
        # with an exception it failed with that exception.  Wakes all waiters.
        def complete!(error=nil)
          @mutex.synchronize do
            @error = error
            @complete = true
            @condition.broadcast
          end
        end

        # Wait until the loading attempt completes.  Raises the load error if the
        # attempt failed.
        def wait
          @mutex.synchronize do
            @condition.wait(@mutex) until @complete
          end

          raise @error.class, @error.message if @error
          nil
        end
      end

      def self.load_dependencies(app)
        app.plugin :hash_branches
      end

      def self.configure(app)
        app.opts[:autoload_hash_branch_files] ||= []
        app.opts[:autoload_hash_branch_states] ||= {}
      end

      module ClassMethods
        RodaPlugins.opt_attr_reader(self, :autoload_hash_branch_files)
        RodaPlugins.opt_attr_reader(self, :autoload_hash_branch_states)

        # Autoload the given file when there is request for the hash branch.
        # The given file should configure the hash branch specified.
        def autoload_hash_branch(namespace='', segment, file)
          segment = "/#{segment}"
          file = File.expand_path(file)
          autoload_hash_branch_files << file
          routes = hash_branches[namespace] ||= {}

          existing_meth = routes[segment]
          state = autoload_hash_branch_states[file] ||= AutoloadState.new
          state.namespace = namespace
          state.segment = segment

          meth = if existing_meth
            define_roda_method(existing_meth, 1) do |r|
              self.class.send(:_roda_load_autoload_hash_branch, file, r)
            end
          else
            define_roda_method("hash_branch_#{namespace}_#{segment}", 1) do |r|
              self.class.send(:_roda_load_autoload_hash_branch, file, r)
            end
          end

          routes[segment] = meth
          state.meth = meth
          state.loader_source_location = instance_method(meth).source_location
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

        # Eagerly load all hash branches when freezing the application.  Each
        # branch is loaded through the same single-flight path used for requests,
        # so a failed load rolls back partially registered branches and propagates
        # the exception.
        def freeze
          unless opts.frozen?
            autoload_hash_branch_files.dup.each do |file|
              send(:_roda_load_autoload_hash_branch!, file)
            end
            opts[:autoload_hash_branch_files] = nil
            opts.delete(:autoload_hash_branch_files)
          end
          super
        end

        private

        # Start the single-flight load of the branch file, returning the loading
        # attempt and whether the caller is the loading thread.  The loading
        # thread runs the load; other threads wait on the returned attempt.
        def _roda_begin_autoload_hash_branch(file)
          state = autoload_hash_branch_states[file]
          recursive_key = :"_roda_autoload_hash_branch_#{state.object_id}"

          state.mutex.synchronize do
            attempt = state.attempt

            if attempt && !attempt.error
              # A load is in progress on another thread.  If this same thread
              # is the one performing the load, the branch file is requesting
              # itself recursively, which can never complete.
              if Thread.current[recursive_key]
                raise RodaError, "recursive autoloading of hash branch file #{file.inspect} attempted while the branch file was being loaded"
              end

              next [attempt, false]
            end

            if Thread.current[recursive_key]
              raise RodaError, "recursive autoloading of hash branch file #{file.inspect} attempted while the branch file was being loaded"
            end

            attempt = AutoloadAttempt.new
            state.attempt = attempt
            Thread.current[recursive_key] = true
            [attempt, true]
          end
        end

        # Finish a successful load.  If the file redefined the routing method,
        # dispatch to the loaded method.  Otherwise leave the loading method in
        # place so the request results in a 404 instead of infinitely recursing.
        def _roda_finish_autoload_hash_branch(file, request)
          state = autoload_hash_branch_states[file]
          meth = state.mutex.synchronize do
            if instance_method(state.meth).source_location == state.loader_source_location
              nil
            else
              state.meth
            end
          end

          request.scope.send(meth, request) if meth
        end

        # Snapshot the hash branch routes before loading a branch file.
        def _roda_autoload_hash_branch_snapshot
          snapshot = {}
          hash_branches.each do |namespace, routes|
            snapshot[namespace] = routes.dup
          end
          snapshot
        end

        # Restore the hash branch routes to the snapshot taken before loading a
        # branch file, so routes the failed file registered or overwrote are not
        # left half-registered.  Reinstalls the loading method for the branch
        # being loaded.
        def _roda_restore_autoload_hash_branches(snapshot, file)
          hash_branches.each do |namespace, routes|
            old_routes = snapshot[namespace]

            if old_routes
              routes.each_key do |segment|
                unless old_routes.key?(segment)
                  meth = routes.delete(segment)
                  if meth && (method_defined?(meth) || private_method_defined?(meth))
                    remove_method(meth)
                  end
                end
              end

              old_routes.each do |segment, meth|
                routes[segment] = meth
              end
            else
              routes.each_value do |meth|
                if method_defined?(meth) || private_method_defined?(meth)
                  remove_method(meth)
                end
              end
              routes.clear
            end
          end

          state = autoload_hash_branch_states[file]
          routes = hash_branches[state.namespace] ||= {}
          routes[state.segment] = state.meth

          define_roda_method(state.meth, 1) do |r|
            self.class.send(:_roda_load_autoload_hash_branch, file, r)
          end
          state.loader_source_location = instance_method(state.meth).source_location
        end

        # Run the loading thread's side of the single-flight load, including
        # rolling back partially registered routes on failure and waking the
        # waiting requests with the result.
        def _roda_run_autoload_hash_branch(file, request, attempt)
          state = autoload_hash_branch_states[file]
          recursive_key = :"_roda_autoload_hash_branch_#{state.object_id}"
          snapshot = _roda_autoload_hash_branch_snapshot

          begin
            require file
            result = _roda_finish_autoload_hash_branch(file, request)
            attempt.complete!
            result
          rescue Exception => e
            _roda_restore_autoload_hash_branches(snapshot, file)
            attempt.complete!(e)
            raise
          ensure
            Thread.current[recursive_key] = nil
          end
        end

        # Single-flight loading entry point used by routing methods.
        def _roda_load_autoload_hash_branch(file, request)
          attempt, loading = _roda_begin_autoload_hash_branch(file)

          if loading
            _roda_run_autoload_hash_branch(file, request, attempt)
          else
            attempt.wait
            _roda_finish_autoload_hash_branch(file, request)
          end
        end

        # Single-flight loading entry point used when eagerly loading branches on
        # freeze, where there is no request to dispatch after loading.
        def _roda_load_autoload_hash_branch!(file)
          state = autoload_hash_branch_states[file]

          completed = state.mutex.synchronize do
            state.attempt && state.attempt.complete? && !state.attempt.error
          end
          return if completed

          attempt, loading = _roda_begin_autoload_hash_branch(file)
          return unless loading

          recursive_key = :"_roda_autoload_hash_branch_#{state.object_id}"
          snapshot = _roda_autoload_hash_branch_snapshot

          begin
            require file
            attempt.complete!
          rescue Exception => e
            _roda_restore_autoload_hash_branches(snapshot, file)
            attempt.complete!(e)
            raise
          ensure
            Thread.current[recursive_key] = nil
          end

          nil
        end
      end
    end

    register_plugin(:autoload_hash_branches, AutoloadHashBranches)
  end
end
