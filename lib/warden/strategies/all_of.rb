# encoding: utf-8
# frozen_string_literal: true
module Warden
  module Strategies
    # An "all of" strategy group.  Unlike the regular strategy chain, where
    # the first successful strategy logs the user in, every member of the
    # group is run and must succeed, and all of the resulting users must
    # identify the same subject before anyone is logged in.
    #
    # Groups are declared with Warden::Strategies.add_all_of, which requires
    # the application to provide a subject matcher block used to decide
    # whether two authenticated users represent the same identity:
    #
    #   Warden::Strategies.add_all_of(:two_factor, [:password, :otp]) do |user_a, user_b|
    #     user_a.id == user_b.id
    #   end
    #
    # The group behaves like a single strategy towards the proxy: the user
    # is only written to the scope (and the authentication callbacks only
    # fire) when the whole group succeeds.  If an early member succeeds but
    # a later one fails, nothing is stored and no half-authenticated session
    # is left behind.
    class AllOf < Base
      class << self
        # The labels of the member strategies that make up the group.
        # :api: private
        attr_accessor :strategy_names

        # The application-provided matcher used to decide whether two
        # authenticated users represent the same subject.
        # :api: private
        attr_accessor :subject_matcher
      end

      # Runs each member strategy in turn.  Succeeds only when every member
      # succeeded and all users identify the same subject.
      # :api: private
      def authenticate!
        users = []

        self.class.strategy_names.each do |name|
          strategy = _run_member(name)

          return fail!(strategy.message || "Failed to Login") if strategy.result == :failure
          return pass unless strategy.successful?

          users << strategy.user
        end

        user = users.first
        if users.drop(1).all? { |other| self.class.subject_matcher.call(user, other) }
          success!(user)
        else
          fail!("Authentication strategies did not identify the same subject")
        end
      end

    private

      def _run_member(name) #:nodoc:
        klass = Warden::Strategies[name]
        raise "Invalid strategy #{name}" unless klass

        strategy = klass.new(env, scope)
        strategy._run! if strategy.valid?
        strategy
      end
    end # AllOf
  end # Strategies
end # Warden
