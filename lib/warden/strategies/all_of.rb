# encoding: utf-8
# frozen_string_literal: true
module Warden
  module Strategies
    # An +AllOf+ strategy is a composite that only succeeds when every member
    # strategy succeeds and every member authenticates the same subject.
    #
    # Unlike the default strategy chain (where the first successful strategy
    # wins), member strategies are all run and keep producing their own
    # results. The user is only written into the scope once:
    #
    #   * if every member succeeds, and
    #   * if the application provided comparison block considers all of the
    #     authenticated subjects equal.
    #
    # As soon as a member fails, halts or authenticates a different subject,
    # the group fails and the user is never set, so no half-authenticated
    # session is left behind and no authentication callback is triggered.
    class AllOf < Base
      # Holds the ordered member strategy labels and the block used to check
      # that the subjects authenticated by each member are the same.
      Group = Struct.new(:strategies, :same_subject)

      # Sentinel value used to distinguish a completed run from a +:warden+
      # throw raised while running a member strategy.
      RAN = Object.new

      # :api: private
      def initialize(env, scope = nil, label = nil, group = nil, &fetch_strategy) #:nodoc:
        super(env, scope)
        @label, @group, @fetch_strategy = label, group, fetch_strategy
      end

      # Runs every member strategy in declaration order. Each member is run
      # exactly like a standalone strategy, but a single shared user is only
      # reported on success of the whole group.
      # :api: private
      def authenticate! #:nodoc:
        first = nil

        @group.strategies.each do |name|
          member = fetch_member(name)
          return fail!("all_of strategy group #{@label.inspect} could not find strategy #{name.inspect}") unless member

          thrown = run_member(member)

          # A member that is not valid for the request, did not run or did
          # not report success cannot satisfy the group.
          unless member.successful?
            if member.result.nil?
              return fail!("all_of strategy group #{@label.inspect} strategy #{name.inspect} did not succeed")
            end
            copy_failure(member)
            throw(:warden, thrown) unless thrown.equal?(RAN)
            return
          end

          if first.nil?
            first = member
          elsif !same_subject?(first.user, member.user)
            return fail!("all_of strategy group #{@label.inspect} authenticated different subjects")
          end
        end

        success!(first.user, first.message)
      end

      private

      def fetch_member(name)
        @fetch_strategy.call(name)
      end

      # Runs a member strategy, catching any :warden throw so the group can
      # adopt the member's result before rethrowing it.
      def run_member(member)
        catch(:warden) do
          member._run!
          RAN
        end
      end

      def same_subject?(first_user, other_user)
        @group.same_subject.call(first_user, other_user)
      end

      # Adopts the result of a failed member so the failure app sees the same
      # message, status, headers and custom response it would for a standalone
      # run of that strategy.
      def copy_failure(member)
        @result = member.result
        @message = member.message
        @status = member.status
        @custom_response = member.custom_response
        headers(member.headers) unless member.headers.empty?
        halt!
      end
    end
  end
end
