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
    def authorize(possibly_namespaced_record, query:, policy_class:)
      record = pundit_model(possibly_namespaced_record)
      policy = if policy_class
        policy_class.new(user, record)
      else
        policy!(possibly_namespaced_record)
      end

      raise NotAuthorizedError, query: query, record: record, policy: policy unless policy.public_send(query)

      record
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

    # Checks several queries for several records in a single batch.
    #
    # Each record is checked against every query, and a matrix is returned
    # that keeps the order of the given records and queries:
    #
    #   context.permissions([post, comment], [:show?, :edit?, :archive?])
    #   # => [[true, false, true], [true, true, false]]
    #
    # Duplicate records keep their position in the matrix, but the policy
    # query is only evaluated once per distinct object identity and query.
    # Two records that share an `id` (or compare equal with `==`) but are
    # distinct objects are evaluated separately. Namespaced records (passed as
    # arrays) and records checked with an explicit `policy_class` take part in
    # their own identities.
    #
    # Query results are never cached between calls, because the underlying
    # records may have changed. Policy instances may be reused for the
    # different queries of a single record within one call.
    #
    # A query returning falsey is a normal denial and stays `false` in the
    # matrix without interrupting the other checks. Programmer errors, on the
    # other hand - a policy that cannot be found, a missing query method or an
    # error raised from inside a policy - raise a {BatchAuthorizationError}
    # carrying the record index and query name.
    #
    # @param records [Array<Object, Array>] the records to check, possibly namespaced
    # @param queries [Array<Symbol, String>] the predicate methods to check on each policy
    # @param policy_class [Class, nil] the policy class to force for every record
    # @raise [BatchAuthorizationError] if a policy is missing, a query is undefined or a policy raises
    # @return [Array<Array<Boolean>>] a records x queries matrix of booleans
    # @since v2.6.0
    def permissions(records, queries, policy_class: nil)
      query_names = queries.map { |query| query.to_sym }

      # Identity => policy instance. A fresh map is used for every call, so
      # neither policies nor their results leak across batches.
      policies = {}
      # [Identity, query] => boolean. De-duplicates repeated object/query pairs
      # within this single call only.
      results = {}

      records.each_with_index.map do |record, index|
        identity = batch_identity(record, policy_class)
        policy = policies[identity] ||= begin
          build_batch_policy(record, policy_class)
        rescue Pundit::Error => error
          raise batch_error(index: index, query: query_names.first, record: pundit_model(record), cause: error)
        end

        query_names.map do |query|
          key = [identity, query]

          results.fetch(key) do
            results[key] = !!policy.public_send(query)
          end
        rescue StandardError => error
          raise batch_error(index: index, query: query, record: pundit_model(record), cause: error)
        end
      end
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

    # Builds the identity used to de-duplicate records within one call to
    # {#permissions}. Identity is based on object identity rather than `==` or
    # `hash`, so two distinct objects with the same id are treated separately,
    # while the namespace and explicit policy class are part of the identity.
    #
    # @api private
    # @since v2.6.0
    def batch_identity(record, policy_class)
      namespace, model =
        if record.is_a?(Array)
          [record[0...-1].map(&:object_id), pundit_model(record)]
        else
          [[], record]
        end

      [namespace, model.object_id, policy_class&.object_id]
    end

    # Instantiates the policy used for a record during a batch check.
    #
    # Unlike {#cached_find}, this never uses the request level policy cache and
    # never stores the resulting policy, so batch results cannot be reused once
    # a record's state has changed.
    #
    # @api private
    # @since v2.6.0
    def build_batch_policy(record, policy_class)
      return policy_class.new(user, pundit_model(record)) if policy_class

      klass = policy_finder(record).policy!

      begin
        klass.new(user, pundit_model(record))
      rescue ArgumentError
        raise InvalidConstructorError, "Invalid #<#{klass}> constructor is called"
      end
    end

    # @api private
    # @since v2.6.0
    def batch_error(index:, query:, record:, cause:)
      BatchAuthorizationError.new(index: index, query: query, record: record, cause: cause)
    end
  end
end
