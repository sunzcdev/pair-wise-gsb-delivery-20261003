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
    # @raise [InvalidConstructorError] if the policy constructor can't accept `(user, record)`
    # @return [Object] Always returns the passed object record
    # @since v2.3.2
    def authorize(possibly_namespaced_record, query:, policy_class:)
      record = pundit_model(possibly_namespaced_record)
      policy = if policy_class
        instantiate_policy(policy_class, record)
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
    # @raise [InvalidConstructorError] if the policy constructor can't accept `(user, record)`
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
    # @raise [InvalidConstructorError] if the policy constructor can't accept `(user, record)`
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
    # @raise [InvalidConstructorError] if the policy scope constructor can't accept `(user, scope)`
    # @return [Scope{#resolve}, nil] instance of scope class which can resolve to a scope
    # @since v2.3.2
    def policy_scope(scope)
      policy_scope_class = policy_finder(scope).scope
      return unless policy_scope_class

      instantiate_policy(policy_scope_class, pundit_model(scope)).resolve
    end

    # Retrieves the policy scope for the given record. Raises if not found.
    #
    # @see https://github.com/varvet/pundit#scopes
    # @param scope [Object] the object we're retrieving the policy scope for
    # @raise [NotDefinedError] if the policy scope cannot be found
    # @raise [InvalidConstructorError] if the policy scope constructor can't accept `(user, scope)`
    # @return [Scope{#resolve}] instance of scope class which can resolve to a scope
    # @since v2.3.2
    def policy_scope!(scope)
      policy_scope_class = policy_finder(scope).scope!

      instantiate_policy(policy_scope_class, pundit_model(scope)).resolve
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
    # @raise [InvalidConstructorError] if the policy constructor can't accept `(user, record)`
    # @since v2.3.2
    def cached_find(record)
      policy_cache.fetch(user: user, record: record) do
        klass = yield policy_finder(record)
        next unless klass

        instantiate_policy(klass, pundit_model(record))
      end
    end

    # Instantiates a policy or policy scope with the user and model.
    #
    # The constructor signature is checked upfront, so an `ArgumentError`
    # raised *inside* the constructor (e.g. by application code) is not
    # mistaken for a constructor mismatch and propagates with its original
    # message and backtrace.
    #
    # @api private
    # @param klass [Class] the policy or policy scope class
    # @param model [Object] the record or scope passed to the constructor
    # @return [Object] an instance of the given class
    # @raise [InvalidConstructorError] if the constructor can't accept `(user, model)`
    # @since v2.5.3
    def instantiate_policy(klass, model)
      raise InvalidConstructorError, invalid_constructor_message(klass) unless constructor_compatible?(klass)

      klass.new(user, model)
    end

    # Checks whether the constructor can be called with two positional
    # arguments (user and record) and no keywords.
    #
    # @api private
    # @param klass [Class] the policy or policy scope class
    # @return [Boolean]
    # @since v2.5.3
    def constructor_compatible?(klass)
      types = klass.instance_method(:initialize).parameters.map(&:first)

      return false if types.include?(:keyreq)

      required = types.count(:req)
      return false if required > 2

      types.include?(:rest) || required + types.count(:opt) >= 2
    end

    # Builds a message describing how Pundit calls the constructor and what
    # the constructor actually accepts.
    #
    # @api private
    # @param klass [Class] the policy or policy scope class
    # @return [String]
    # @since v2.5.3
    def invalid_constructor_message(klass)
      signature = klass.instance_method(:initialize).parameters.map do |type, name|
        format_parameter(type, name)
      end.join(", ")

      "Invalid #<#{klass}> constructor is called. " \
        "Pundit calls it with (user, record), but its signature is (#{signature})"
    end

    # @api private
    # @since v2.5.3
    def format_parameter(type, name)
      case type
      when :req then name.to_s
      when :opt then "#{name} = ?"
      when :rest then name ? "*#{name}" : "*"
      when :keyreq then "#{name}:"
      when :key then "#{name}: ?"
      when :keyrest then name ? "**#{name}" : "**"
      when :nokey then "**nil"
      when :block then name ? "&#{name}" : "&"
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
