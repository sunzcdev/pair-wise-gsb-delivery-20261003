# frozen_string_literal: true

module Pundit
  # {Pundit::Context} is intended to be created once per request and user, and
  # it is then used to perform authorization checks throughout the request.
  #
  # @example Using Sinatra
  #   helpers do
  #     def current_user = ...
  #
  #     def pundit
  #       @pundit ||= Pundit::Context.new(user: current_user)
  #     end
  #   end
  #
  #   get "/posts/:id" do |id|
  #     pundit.authorize(Post.find(id), query: :show?)
  #   end
  #
  # @example Using [Roda](https://roda.jeremyevans.net/index.html)
  #   route do |r|
  #     context = Pundit::Context.new(user:)
  #
  #     r.get "posts", Integer do |id|
  #       context.authorize(Post.find(id), query: :show?)
  #     end
  #   end
  #
  # @since v2.3.2
  class Context
    # @see Pundit::Authorization#pundit
    # @param user later passed to policies and scopes
    # @param policy_cache [#fetch] cache store for policies (see e.g. {CacheStore::NullStore})
    # @since v2.3.2
    def initialize(user:, policy_cache: CacheStore::NullStore.instance)
      @user = user
      @policy_cache = policy_cache
      @batch_policies = {}
    end

    # @api public
    # @see #initialize
    # @since v2.3.2
    attr_reader :user

    # @api private
    # @see #initialize
    # @since v2.3.2
    attr_reader :policy_cache

    # @!group Policies

    # Retrieves the policy for the given record, initializing it with the
    # record and user and finally throwing an error if the user is not
    # authorized to perform the given action.
    #
    # @param possibly_namespaced_record [Object, Array] the object we're checking permissions of
    # @param query [Symbol, String] the predicate method to check on the policy (e.g. `:show?`)
    # @param policy_class [Class] the policy class we want to force use of
    # @raise [NotAuthorizedError] if the given query method returned false
    # @return [Object] Always returns the passed object record
    # @since v2.3.2
    def authorize(possibly_namespaced_record, query:, policy_class: nil)
      record = pundit_model(possibly_namespaced_record)
      policy = policy_for(possibly_namespaced_record, policy_class)

      raise NotAuthorizedError, query: query, record: record, policy: policy unless policy.public_send(query)

      record
    end

    # Evaluates multiple policy queries for multiple records.
    #
    # Query results are only deduplicated within this call. Policy instances may
    # be reused, but permissions are not cached between calls.
    #
    # @param records [Enumerable] records to check permissions for
    # @param queries [Enumerable<Symbol, String>] predicate methods to check
    # @param policy_class [Class] the policy class we want to force use of
    # @raise [BatchAuthorizationError] if a query cannot be evaluated
    # @return [Array<Array(Boolean)>] matrix of query results in the given order
    # @since v2.6.0
    def policy_permissions(records, queries, policy_class: nil)
      query_names = queries.map { |query| query.to_sym }
      results = {}

      records.each.with_index.map do |record, record_index|
        identity = batch_policy_identity(record, policy_class)

        query_names.map do |query|
          result_key = [identity, query]
          next results[result_key] if results.key?(result_key)

          policy = @batch_policies.fetch(identity) do
            @batch_policies[identity] = find_batch_policy(record, policy_class, record_index, query)
          end

          unless policy.respond_to?(query)
            raise BatchAuthorizationError.new(
              record_index: record_index,
              query: query,
              record: pundit_model(record)
            )
          end

          results[result_key] = begin
            !!policy.public_send(query)
          rescue StandardError
            raise BatchAuthorizationError.new(
              record_index: record_index,
              query: query,
              record: pundit_model(record)
            )
          end
        end
      end
    end

    # Retrieves the policy for the given record.
    #
    # @see https://github.com/varvet/pundit#policies
    # @param record [Object] the object we're retrieving the policy for
    # @raise [InvalidConstructorError] if the policy constructor called incorrectly
    # @return [Object, nil] instance of policy class with query methods
    # @since v2.3.2
    def policy(record)
      cached_find(record, &:policy)
    end

    # Retrieves the policy for the given record, or raises if not found.
    #
    # @see https://github.com/varvet/pundit#policies
    # @param record [Object] the object we're retrieving the policy for
    # @raise [NotDefinedError] if the policy cannot be found
    # @raise [InvalidConstructorError] if the policy constructor called incorrectly
    # @return [Object] instance of policy class with query methods
    # @since v2.3.2
    def policy!(record)
      cached_find(record, &:policy!)
    end

    # @!endgroup

    # @!group Scopes

    # Retrieves the policy scope for the given record.
    #
    # @see https://github.com/varvet/pundit#scopes
    # @param scope [Object] the object we're retrieving the policy scope for
    # @raise [InvalidConstructorError] if the policy constructor called incorrectly
    # @return [Scope{#resolve}, nil] instance of scope class which can resolve to a scope
    # @since v2.3.2
    def policy_scope(scope)
      policy_scope_class = policy_finder(scope).scope
      return unless policy_scope_class

      begin
        policy_scope = policy_scope_class.new(user, pundit_model(scope))
      rescue ArgumentError
        raise InvalidConstructorError, "Invalid #<#{policy_scope_class}> constructor is called"
      end

      policy_scope.resolve
    end

    # Retrieves the policy scope for the given record. Raises if not found.
    #
    # @see https://github.com/varvet/pundit#scopes
    # @param scope [Object] the object we're retrieving the policy scope for
    # @raise [NotDefinedError] if the policy scope cannot be found
    # @raise [InvalidConstructorError] if the policy constructor called incorrectly
    # @return [Scope{#resolve}] instance of scope class which can resolve to a scope
    # @since v2.3.2
    def policy_scope!(scope)
      policy_scope_class = policy_finder(scope).scope!

      begin
        policy_scope = policy_scope_class.new(user, pundit_model(scope))
      rescue ArgumentError
        raise InvalidConstructorError, "Invalid #<#{policy_scope_class}> constructor is called"
      end

      policy_scope.resolve
    end

    # @!endgroup

    private

    # @!group Private Helpers

    # Finds a cached policy for the given record, or yields to find one.
    #
    # @api private
    # @param record [Object] the object we're retrieving the policy for
    # @yield a policy finder if no policy was cached
    # @yieldparam [PolicyFinder] policy_finder
    # @yieldreturn [#new(user, model)]
    # @return [Policy, nil] an instantiated policy
    # @raise [InvalidConstructorError] if policy can't be instantated
    # @since v2.3.2
    def cached_find(record)
      policy_cache.fetch(user: user, record: record) do
        klass = yield policy_finder(record)
        next unless klass

        model = pundit_model(record)

        begin
          klass.new(user, model)
        rescue ArgumentError
          raise InvalidConstructorError, "Invalid #<#{klass}> constructor is called"
        end
      end
    end

    def policy_for(record, policy_class)
      if policy_class
        instantiate_policy(record, policy_class)
      else
        policy!(record)
      end
    end

    def instantiate_policy(record, policy_class)
      policy_class.new(user, pundit_model(record))
    rescue ArgumentError
      raise InvalidConstructorError, "Invalid #<#{policy_class}> constructor is called"
    end

    def find_batch_policy(record, policy_class, record_index, query)
      klass = policy_class || policy_finder(record).policy!

      begin
        klass.new(user, pundit_model(record))
      rescue ArgumentError
        raise InvalidConstructorError, "Invalid #<#{klass}> constructor is called"
      end
    rescue StandardError
      raise BatchAuthorizationError.new(
        record_index: record_index,
        query: query,
        record: pundit_model(record)
      )
    end

    def batch_policy_identity(record, policy_class)
      if record.is_a?(Array)
        [policy_class, record[0...-1], pundit_model(record).object_id]
      else
        [policy_class, record.object_id]
      end
    end

    # Return a policy finder for the given record.
    #
    # @api private
    # @return [PolicyFinder]
    # @since v2.3.2
    def policy_finder(record)
      PolicyFinder.new(record)
    end

    # Given a possibly namespaced record, return the actual record.
    #
    # @api private
    # @since v2.3.2
    def pundit_model(record)
      record.is_a?(Array) ? record.last : record
    end
  end
end
