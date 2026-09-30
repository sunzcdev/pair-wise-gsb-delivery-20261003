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
      @winning_strategy_names, @auth_evidence = {}, {}
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
      throw(:warden, opts) unless user
      user
    end

    # Check to see if there is an authenticated user for the given scope.
    # This brings the user from the session, but does not run strategies before doing so.
    # If you want strategies to be run, please check authenticate?.
    #
    # Beyond mere presence of a user, evidence requirements may be given.
    # They never run strategies and never refresh the recorded authentication
    # time; they only inspect the stored evidence:
    #   env['warden'].authenticated?(:admin, :methods => [:otp], :max_age => 300)
    #
    # Parameters:
    #   args - an optional scope symbol followed by an options hash
    #   opts - :scope, :methods (one or more acceptable strategy names),
    #          :method (alias for a single method), :max_age (seconds),
    #          :clock (an object responding to #now, defaults to Time)
    #
    # Example:
    #   env['warden'].authenticated?(:admin)
    #
    # :api: public
    def authenticated?(*args)
      scope, opts = _retrieve_check_scope_and_opts(args)
      result = !!user(scope) && _evidence_satisfied?(scope, opts)
      yield if block_given? && result
      result
    end

    # Passive check for whether the scope's stored authentication evidence
    # (method and completion time) satisfies the given requirements. Unlike
    # authenticate?, this does not attempt to authenticate when the evidence
    # is missing or stale, and it never refreshes the recorded time.
    #
    # Example:
    #   env['warden'].reauthenticated?(:methods => :otp, :max_age => 300)
    #
    # :api: public
    def reauthenticated?(*args)
      scope, opts = _retrieve_check_scope_and_opts(args)
      !!user(scope) && _evidence_satisfied?(scope, opts)
    end
    alias recently_authenticated? reauthenticated?

    # Step-up entry point for sensitive actions. Returns the user only when
    # the requested evidence (a specific method and/or a maximum age) is
    # available; otherwise it throws a :warden symbol, just like
    # authenticate!, so the request is halted and rendered through the
    # failure app.
    #
    # When evidence is missing or stale, strategies listed in the call are
    # attempted first (allowing a fresh strong authentication during the same
    # request). A plain, still-valid login is left untouched: the user stays
    # logged in, but this sensitive entry reports the requirements as
    # unmet.
    #
    # Example:
    #   env['warden'].authenticate_recently!(:otp, :max_age => 300)
    #   env['warden'].authenticate_recently!(:methods => :otp, :max_age => 300)
    #
    # :api: public
    def authenticate_recently!(*args)
      user, opts = _perform_authentication(*args)
      throw(:warden, opts) unless user
      user
    end
    alias reauthenticate! authenticate_recently!

    # Returns the recorded authentication evidence for a scope (method and
    # completion time), or nil when the scope never authenticated through a
    # strategy during this session. Restoring the user from the session does
    # not create evidence.
    #
    # :api: public
    def auth_evidence(scope = @config.default_scope)
      @auth_evidence[scope] ||=
        Warden::AuthEvidence.from_session(raw_auth_evidence[scope.to_s])
    end

    # @return [Symbol, nil] the strategy name that last authenticated the scope
    # :api: public
    def authentication_method(scope = @config.default_scope)
      evidence = auth_evidence(scope)
      evidence && evidence.method
    end

    # @return [Time, nil] when the scope last completed an authentication
    # :api: public
    def last_authentication_at(scope = @config.default_scope)
      evidence = auth_evidence(scope)
      evidence && Time.at(evidence.at)
    end

    # Same API as authenticated?, but returns false when authenticated.
    # :api: public
    def unauthenticated?(scope = @config.default_scope)
      result = !authenticated?(scope)
      yield if block_given? && result
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

      # Only a genuine authentication carries evidence of how it happened.
      # Fetching an existing user keeps the previously recorded evidence;
      # any other manual assignment clears it unless a method is supplied.
      if opts[:event] == :authentication
        _record_auth_evidence(scope, opts[:method], opts)
      elsif opts[:event] != :fetch
        _clear_auth_evidence(scope)
      end

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
    # :api: public
    def user(argument = {})
      opts  = argument.is_a?(Hash) ? argument : { :scope => argument }
      scope = (opts[:scope] ||= @config.default_scope)

      if @users.has_key?(scope)
        @users[scope]
      else
        unless user = session_serializer.fetch(scope)
          run_callbacks = opts.fetch(:run_callbacks, true)
          manager._run_callbacks(:after_failed_fetch, user, self, :scope => scope) if run_callbacks
        end

        @users[scope] = user ? set_user(user, opts.merge(:event => :fetch)) : nil
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
        scopes = @users.keys
        reset_session = true
      end

      scopes.each do |scope|
        user = @users.delete(scope)
        manager._run_callbacks(:before_logout, user, self, :scope => scope)

        raw_session.delete("warden.user.#{scope}.session") unless raw_session.nil?
        _clear_auth_evidence(scope)
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
      # If the evidence already satisfies the requested method/max-age
      # requirements, the cached user is returned without running any
      # strategy and without refreshing the recorded time.
      existing = user(opts.merge(:scope => scope))
      if existing
        # No evidence requirements: the plain login is always usable.
        return existing, opts if _no_evidence_requirements?(opts)
        return existing, opts if _evidence_satisfied?(scope, opts)
      end

      # If there was no user in the session, see if we can get one from the request.
      _run_strategies_for(scope, args)

      if winning_strategy && winning_strategy.successful?
        opts[:store] = opts.fetch(:store, winning_strategy.store?)
        candidate_method = @winning_strategy_names[scope]
        if _candidate_method_allowed?(opts, candidate_method)
          set_user(
            winning_strategy.user,
            opts.merge!(:event => :authentication,
                        :method => candidate_method)
          )
        end
      end

      [_effective_user(scope, opts), opts]
    end

    def _retrieve_scope_and_opts(args) #:nodoc:
      opts  = args.last.is_a?(Hash) ? args.pop : {}
      scope = opts[:scope] || @config.default_scope
      opts  = (@config[:scope_defaults][scope] || {}).merge(opts)
      [scope, opts]
    end

    # Like _retrieve_scope_and_opts, but also accepts a bare scope symbol as
    # the first positional argument (the authenticated? style).
    def _retrieve_check_scope_and_opts(args) #:nodoc:
      if args.first.is_a?(Symbol)
        scope_from_arg = args.shift
        args << {} unless args.last.is_a?(Hash)
        args.last[:scope] ||= scope_from_arg
      end
      _retrieve_scope_and_opts(args)
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
          _update_winning_strategy(strategy, scope, name)
        end

        strategy._run!
        _update_winning_strategy(strategy, scope, name)
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
    def _update_winning_strategy(strategy, scope, name = nil)
      self.winning_strategy = @winning_strategies[scope] = strategy
      @winning_strategy_names[scope] = name.to_sym if name
    end

    # Extracts evidence requirements from merged options.
    # :api: private
    def _auth_requirements(opts)
      methods = opts[:methods] || opts[:method]
      methods = Array(methods) unless methods.nil?
      {
        :methods => methods,
        :max_age => opts[:max_age],
        :clock   => opts[:clock] || @config[:clock] || Time
      }
    end

    # True when the call imposes no method or freshness requirements.
    # :api: private
    def _no_evidence_requirements?(opts)
      requirements = _auth_requirements(opts)
      requirements[:methods].nil? && requirements[:max_age].nil?
    end

    # Whether a candidate strategy name is acceptable for the call. Age is
    # intentionally not considered here: a fresh success always satisfies the
    # age requirement, but a weak method can never stand in for a strong one.
    # :api: private
    def _candidate_method_allowed?(opts, method)
      methods = _auth_requirements(opts)[:methods]
      if methods
        return false unless method && methods.map(&:to_sym).include?(method.to_sym)
      end

      # A success completed "now" is always fresh enough for any max age.
      true
    end

    # The user a caller should observe: always present for a plain login,
    # but hidden when step-up requirements are not satisfied.
    # :api: private
    def _effective_user(scope, opts)
      return @users[scope] if _no_evidence_requirements?(opts)
      _evidence_satisfied?(scope, opts) ? @users[scope] : nil
    end

    # Whether the stored evidence for a scope meets the requested method and
    # age. No requirements means any evidence (or none) is acceptable.
    # :api: private
    def _evidence_satisfied?(scope, opts)
      requirements = _auth_requirements(opts)
      # No evidence requirements: an already logged-in user is enough, even
      # for sessions that predate evidence tracking (plain :authenticated?).
      return true if requirements[:methods].nil? && requirements[:max_age].nil?

      evidence = auth_evidence(scope)
      return false unless evidence

      evidence.satisfies?(**requirements)
    end

    # Persists fresh authentication evidence for a scope.
    # :api: private
    def _record_auth_evidence(scope, method, opts = {})
      return unless method

      clock = (opts[:clock] || @config[:clock] || Time)
      evidence = Warden::AuthEvidence.record(method, clock)
      @auth_evidence[scope] = evidence
      raw_auth_evidence[scope.to_s] = evidence.to_session unless opts[:store] == false
      evidence
    end

    # Removes the stored authentication evidence for a scope.
    # :api: private
    def _clear_auth_evidence(scope)
      @auth_evidence.delete(scope)
      raw_auth_evidence.delete(scope.to_s) unless raw_session.nil?
    end

    # Hash of per-scope evidence held in the session.
    # :api: private
    def raw_auth_evidence
      return {} if raw_session.nil?
      raw_session[Warden::AuthEvidence::SESSION_KEY] ||= {}
    end
  end # Proxy

end # Warden
