# encoding: utf-8
# frozen_string_literal: true

module Warden
  class UserNotSet < RuntimeError; end

  class Proxy
    # An accessor to the winning strategy
    # :api: private
    attr_accessor :winning_strategy

    # An accessor to the rack env hash, the proxy owner and its config
    # :api: public
    attr_reader :env, :manager, :config, :winning_strategies

    extend ::Forwardable
    include ::Warden::Mixins::Common

    ENV_WARDEN_ERRORS = 'warden.errors'.freeze
    ENV_SESSION_OPTIONS = 'rack.session.options'.freeze

    # :api: private
    def_delegators :winning_strategy, :headers, :status, :custom_response

    # :api: public
    def_delegators :config, :default_strategies

    def initialize(env, manager) #:nodoc:
      @env, @users, @winning_strategies, @locked = env, {}, {}, false
      @manager, @config = manager, manager.config.dup
      @strategies = Hash.new { |h,k| h[k] = {} }
      # Tri-state session restore results per scope
      # (Warden::SessionRestoreResult), kept separate from @users so a
      # transient "identity store unavailable" is never confused with a
      # genuinely missing identity.
      @restore_results = {}
    end

    # Run the on_request callbacks
    # :api: private
    def on_request
      manager._run_callbacks(:on_request, self)
    end

    # Lazily initiate errors object in session.
    # :api: public
    def errors
      @env[ENV_WARDEN_ERRORS] ||= Errors.new
    end

    # Points to a SessionSerializer instance responsible for handling
    # everything related with storing, fetching and removing the user
    # session.
    # :api: public
    def session_serializer
      @session_serializer ||= Warden::SessionSerializer.new(@env)
    end

    # Clear the cache of performed strategies so far. Warden runs each
    # strategy just once during the request lifecycle. You can clear the
    # strategies cache if you want to allow a strategy to be run more than
    # once.
    #
    # This method has the same API as authenticate, allowing you to clear
    # specific strategies for given scope:
    #
    # Parameters:
    #   args - a list of symbols (labels) that name the strategies to attempt
    #   opts - an options hash that contains the :scope of the user to check
    #
    # Example:
    #   # Clear all strategies for the configured default_scope
    #   env['warden'].clear_strategies_cache!
    #
    #   # Clear all strategies for the :admin scope
    #   env['warden'].clear_strategies_cache!(:scope => :admin)
    #
    #   # Clear password strategy for the :admin scope
    #   env['warden'].clear_strategies_cache!(:password, :scope => :admin)
    #
    # :api: public
    def clear_strategies_cache!(*args)
      scope, _opts = _retrieve_scope_and_opts(args)

      @winning_strategies.delete(scope)
      @strategies[scope].each do |k, v|
        v.clear! if args.empty? || args.include?(k)
      end
    end

    # Locks the proxy so new users cannot authenticate during the
    # request lifecycle. This is useful when the request cannot
    # be verified (for example, using a CSRF verification token).
    # Notice that already authenticated users are kept as so.
    #
    # :api: public
    def lock!
      @locked = true
    end

    # Run the authentication strategies for the given strategies.
    # If there is already a user logged in for a given scope, the strategies are not run
    # This does not halt the flow of control and is a passive attempt to authenticate only
    # When scope is not specified, the default_scope is assumed.
    #
    # Parameters:
    #   args - a list of symbols (labels) that name the strategies to attempt
    #   opts - an options hash that contains the :scope of the user to check
    #
    # Example:
    #   env['warden'].authenticate(:password, :basic, :scope => :sudo)
    #
    # :api: public
    def authenticate(*args)
      user, _opts = _perform_authentication(*args)
      user
    end

    # Same API as authenticate, but returns a boolean instead of a user.
    # The difference between this method (authenticate?) and authenticated?
    # is that the former will run strategies if the user has not yet been
    # authenticated, and the second relies on already performed ones.
    # :api: public
    def authenticate?(*args)
      result = !!authenticate(*args)
      yield if result && block_given?
      result
    end

    # The same as +authenticate+ except on failure it will throw a :warden symbol causing the request to be halted
    # and rendered through the +failure_app+
    #
    # Example
    #   env['warden'].authenticate!(:password, :scope => :publisher) # throws if it cannot authenticate
    #
    # :api: public
    def authenticate!(*args)
      user, opts = _perform_authentication(*args)
      unless user
        # Surface a temporarily unavailable identity store to the failure
        # entry point so it can react differently from a plain missing
        # session. No other strategy was run in that case.
        scope = opts[:scope] || @config.default_scope
        opts = opts.merge(:reason => :identity_store_unavailable) if session_unavailable?(scope)
        throw(:warden, opts)
      end
      user
    end

    # Check to see if there is an authenticated user for the given scope.
    # This brings the user from the session, but does not run strategies before doing so.
    # If you want strategies to be run, please check authenticate?.
    #
    # Parameters:
    #   scope - the scope to check for authentication. Defaults to default_scope
    #
    # Example:
    #   env['warden'].authenticated?(:admin)
    #
    # :api: public
    def authenticated?(scope = @config.default_scope)
      result = !!user(scope)
      yield if block_given? && result
      result
    end

    # Same API as authenticated?, but returns false when authenticated.
    # :api: public
    def unauthenticated?(scope = @config.default_scope)
      result = !authenticated?(scope)
      yield if block_given? && result
      result
    end

    # Returns whether the session for the given scope could not be restored
    # because the identity store was temporarily unavailable.
    #
    # Unlike a missing/deleted identity this state is recoverable: the
    # original session value was preserved and no other authentication
    # strategy was attempted. Failure entry points (failure apps,
    # +before_failure+ callbacks) can use this to distinguish an outage from
    # an unauthenticated request.
    #
    # Parameters:
    #   scope - the scope to check. Defaults to default_scope
    #
    # Example:
    #   env['warden'].session_unavailable?
    #
    # :api: public
    def session_unavailable?(scope = @config.default_scope)
      scope = scope[:scope] if scope.is_a?(Hash)
      result = @restore_results[scope]
      result ? result.unavailable? : false
    end

    # Returns the Warden::SessionRestoreResult describing the last attempt to
    # restore the session for the given scope, or +nil+ if no attempt has been
    # made yet during this request.
    #
    # The result distinguishes +:restored+, +:missing+ and +:unavailable+.
    #
    # :api: public
    def session_restore_result(scope = @config.default_scope)
      scope = scope[:scope] if scope.is_a?(Hash)
      @restore_results[scope]
    end

    # Forces a fresh attempt to restore the session for the given scope,
    # bypassing the per-request restore cache.
    #
    # This is useful after the identity store has recovered from a transient
    # outage: the preserved session value can be retried within the same
    # request. A genuinely deleted identity stays missing and cannot be
    # "brought back" by retrying, because its session value was deleted.
    #
    # Parameters:
    #   opts - an options hash that contains the :scope of the user
    #
    # Example:
    #   env['warden'].restore_session!(:scope => :admin)
    #
    # :api: public
    def restore_session!(opts = {})
      opts  = opts.is_a?(Hash) ? opts : { :scope => opts }
      scope = (opts[:scope] ||= @config.default_scope)
      result = _restore_session(scope, :force => true)
      # Keep the per-request user cache in sync with the fresh result so a
      # later #user reflects the retry. Unavailable results leave @users
      # untouched (no nil is cached).
      _apply_restore_result(result, opts)
      result
    end

    # Manually set the user into the session and auth proxy
    #
    # Parameters:
    #   user - An object that has been setup to serialize into and out of the session.
    #   opts - An options hash.  Use the :scope option to set the scope of the user, set the :store option to false to skip serializing into the session, set the :run_callbacks to false to skip running the callbacks (the default is true).
    #
    # :api: public
    def set_user(user, opts = {})
      scope = (opts[:scope] ||= @config.default_scope)

      # Get the default options from the master configuration for the given scope
      opts = (@config[:scope_defaults][scope] || {}).merge(opts)
      opts[:event] ||= :set_user
      @users[scope] = user

      if opts[:store] != false && opts[:event] != :fetch
        options = env[ENV_SESSION_OPTIONS]
        if options
          if options.frozen?
            env[ENV_SESSION_OPTIONS] = options.merge(:renew => true).freeze
          else
            options[:renew] = true
          end
        end
        session_serializer.store(user, scope)
      end

      run_callbacks = opts.fetch(:run_callbacks, true)
      manager._run_callbacks(:after_set_user, user, self, opts) if run_callbacks

      @users[scope]
    end

    # Provides access to the user object in a given scope for a request.
    # Will be nil if not logged in. Please notice that this method does not
    # perform strategies.
    #
    # Example:
    #   # without scope (default user)
    #   env['warden'].user
    #
    #   # with scope
    #   env['warden'].user(:admin)
    #
    #   # as a Hash
    #   env['warden'].user(:scope => :admin)
    #
    #   # with default scope and run_callbacks option
    #   env['warden'].user(:run_callbacks => false)
    #
    #  # with a scope and run_callbacks option
    #  env['warden'].user(:scope => :admin, :run_callbacks => true)
    #
    #  # force a fresh session restore (bypassing the per-request cache)
    #  env['warden'].user(:restore => true)
    #
    # :api: public
    def user(argument = {})
      opts  = argument.is_a?(Hash) ? argument : { :scope => argument }
      scope = (opts[:scope] ||= @config.default_scope)

      if opts[:restore]
        # Explicit request to re-run session restoration, ignoring the
        # cached restore result and any previously cached user for the scope.
        _apply_restore_result(_restore_session(scope, :force => true), opts)
      elsif @users.has_key?(scope)
        # A user that was set manually / via a strategy, or a previously
        # resolved user (including a cached nil for a missing identity).
        @users[scope]
      else
        # First look for this scope during the request. A cached
        # +:unavailable+ result is reused so the store is not hammered and
        # the outage is not confused with a deletion.
        result = @restore_results[scope] || _restore_session(scope)
        _apply_restore_result(result, opts)
      end
    end

    # Provides a scoped session data for authenticated users.
    # Warden manages clearing out this data when a user logs out
    #
    # Example
    #  # default scope
    #  env['warden'].session[:foo] = "bar"
    #
    #  # :sudo scope
    #  env['warden'].session(:sudo)[:foo] = "bar"
    #
    # :api: public
    def session(scope = @config.default_scope)
      raise NotAuthenticated, "#{scope.inspect} user is not logged in" unless authenticated?(scope)
      raw_session["warden.user.#{scope}.session"] ||= {}
    end

    # Provides logout functionality.
    # The logout also manages any authenticated data storage and clears it when a user logs out.
    #
    # Parameters:
    #   scopes - a list of scopes to logout
    #
    # Example:
    #  # Logout everyone and clear the session
    #  env['warden'].logout
    #
    #  # Logout the default user but leave the rest of the session alone
    #  env['warden'].logout(:default)
    #
    #  # Logout the :publisher and :admin user
    #  env['warden'].logout(:publisher, :admin)
    #
    # :api: public
    def logout(*scopes)
      if scopes.empty?
        scopes = @users.keys | @restore_results.keys
        reset_session = true
      end

      scopes.each do |scope|
        user = @users.delete(scope)
        @restore_results.delete(scope)
        manager._run_callbacks(:before_logout, user, self, :scope => scope)

        raw_session.delete("warden.user.#{scope}.session") unless raw_session.nil?
        session_serializer.delete(scope, user)
      end

      reset_session! if reset_session
    end

    # proxy methods through to the winning strategy
    # :api: private
    def result # :nodoc:
      winning_strategy && winning_strategy.result
    end

    # Proxy through to the authentication strategy to find out the message that was generated.
    # :api: public
    def message
      winning_strategy && winning_strategy.message
    end

    # Provides a way to return a 401 without warden deferring to the failure app
    # The result is a direct passthrough of your own response
    # :api: public
    def custom_failure!
      @custom_failure = true
    end

    # Check to see if the custom failure flag has been set
    # :api: public
    def custom_failure?
      if instance_variable_defined?(:@custom_failure)
        !!@custom_failure
      else
        false
      end
    end

    # Check to see if this is an asset request
    # :api: public
    def asset_request?
      ::Warden::asset_paths.any? { |r| env['PATH_INFO'].to_s.match(r) }
    end

    def inspect(*args)
      "Warden::Proxy:#{object_id} @config=#{@config.inspect}"
    end

    def to_s(*args)
      inspect(*args)
    end

    private

    def _perform_authentication(*args)
      scope, opts = _retrieve_scope_and_opts(args)
      user = nil

      # Look for an existing user in the session for this scope.
      # If there was no user in the session, see if we can get one from the request.
      return user, opts if user = user(opts.merge(:scope => scope))

      # If the session could not be restored because the identity store is
      # temporarily unavailable, do not silently fall back to other
      # authentication strategies: the preserved session may still be valid
      # and treating the outage as "not logged in" would sign users out.
      unless session_unavailable?(scope)
        _run_strategies_for(scope, args)

        if winning_strategy && winning_strategy.successful?
          opts[:store] = opts.fetch(:store, winning_strategy.store?)
          set_user(winning_strategy.user, opts.merge!(:event => :authentication))
        end
      end

      [@users[scope], opts]
    end

    # Runs the serializer's tri-state session restore for the scope and caches
    # the result. With :force the cache is bypassed so an application can
    # retry after the identity store has recovered.
    def _restore_session(scope, force: false)
      @restore_results[scope] = session_serializer.restore(scope) if force || !@restore_results.has_key?(scope)
      @restore_results[scope]
    end

    # Applies a Warden::SessionRestoreResult to the per-scope user cache.
    # A restored user is set with the :fetch event; a missing identity is
    # cached as nil and triggers after_failed_fetch; an unavailable store
    # leaves the session and the user cache untouched.
    def _apply_restore_result(result, opts)
      scope = opts[:scope]

      if result.restored?
        @users[scope] = set_user(result.user, opts.merge(:event => :fetch))
      elsif result.missing?
        @users[scope] = nil
        run_callbacks = opts.fetch(:run_callbacks, true)
        manager._run_callbacks(:after_failed_fetch, nil, self, :scope => scope) if run_callbacks
        nil
      else
        # :unavailable - never populate @users with nil and never delete the
        # session; the failure entry point identifies this via
        # session_unavailable?.
        nil
      end
    end

    def _retrieve_scope_and_opts(args) #:nodoc:
      opts  = args.last.is_a?(Hash) ? args.pop : {}
      scope = opts[:scope] || @config.default_scope
      opts  = (@config[:scope_defaults][scope] || {}).merge(opts)
      [scope, opts]
    end

    # Run the strategies for a given scope
    def _run_strategies_for(scope, args) #:nodoc:
      self.winning_strategy = @winning_strategies[scope]
      return if winning_strategy && winning_strategy.halted?

      # Do not run any strategy if locked
      return if @locked

      if args.empty?
        defaults   = @config[:default_strategies]
        strategies = defaults[scope] || defaults[:_all]
      end

      (strategies || args).each do |name|
        strategy = _fetch_strategy(name, scope)
        next unless strategy && !strategy.performed? && strategy.valid?
        catch(:warden) do
          _update_winning_strategy(strategy, scope)
        end

        strategy._run!
        _update_winning_strategy(strategy, scope)
        break if strategy.halted?
      end
    end

    # Fetches strategies and keep them in a hash cache.
    def _fetch_strategy(name, scope)
      @strategies[scope][name] ||= if klass = Warden::Strategies[name]
        klass.new(@env, scope)
      elsif @config.silence_missing_strategies?
        nil
      else
        raise "Invalid strategy #{name}"
      end
    end

    # Updates the winning strategy for a given scope
    def _update_winning_strategy(strategy, scope)
      self.winning_strategy = @winning_strategies[scope] = strategy
    end
  end # Proxy

end # Warden
