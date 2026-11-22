#
# Fluentd
#
#    Licensed under the Apache License, Version 2.0 (the "License");
#    you may not use this file except in compliance with the License.
#    You may obtain a copy of the License at
#
#        http://www.apache.org/licenses/LICENSE-2.0
#
#    Unless required by applicable law or agreed to in writing, software
#    distributed under the License is distributed on an "AS IS" BASIS,
#    WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
#    See the License for the specific language governing permissions and
#    limitations under the License.
#

require 'fluent/plugin/input'

module Fluent::Plugin
  class TailInput < Fluent::Plugin::Input
    module GroupWatchParams
      include Fluent::Configurable

      DEFAULT_KEY = /.*/
      DEFAULT_LIMIT = -1
      REGEXP_JOIN = "_"

      config_section :group, param_name: :group, required: false, multi: false do
        desc 'Regex for extracting group\'s metadata'
        config_param :pattern,
                     :regexp,
                     default: /^\/var\/log\/containers\/(?<podname>[a-z0-9]([-a-z0-9]*[a-z0-9])?(\/[a-z0-9]([-a-z0-9]*[a-z0-9])?)*)_(?<namespace>[^_]+)_(?<container>.+)-(?<docker_id>[a-z0-9]{64})\.log$/

        desc 'Period of time in which the group_line_limit is applied'
        config_param :rate_period, :time, default: 5

        config_section :rule, param_name: :rule, required: true, multi: true do
          desc 'Key-value pairs for grouping'
          config_param :match, :hash, value_type: :regexp, default: { namespace: [DEFAULT_KEY], podname: [DEFAULT_KEY] }
          desc 'Maximum number of log lines allowed per group over a period of rate_period'
          config_param :limit, :integer, default: DEFAULT_LIMIT
        end
      end
    end

    module GroupWatch
      def self.included(mod)
        mod.include GroupWatchParams
      end

      attr_reader :group_watchers, :default_group_key

      def initialize
        super
        @group_watchers = {}
        @group_keys = nil
        @default_group_key = nil
      end

      def configure(conf)
        super

        unless @group.nil?
          ## Ensuring correct time period syntax
          @group.rule.each { |rule|
            raise "Metadata Group Limit >= DEFAULT_LIMIT" unless rule.limit >= GroupWatchParams::DEFAULT_LIMIT
          }

          @group_keys = Regexp.compile(@group.pattern).named_captures.keys
          @default_group_key = ([GroupWatchParams::DEFAULT_KEY] * @group_keys.length).join(GroupWatchParams::REGEXP_JOIN)

          ## Ensures that "specific" rules (with larger number of `rule.match` keys)
          ## have a higher priority against "generic" rules (with less number of `rule.match` keys).
          ## This will be helpful when a file satisfies more than one rule.
          @group.rule.sort_by! { |rule| -rule.match.length() }
          construct_groupwatchers
          @group_watchers[@default_group_key] ||= GroupWatcher.new(@group.rate_period, GroupWatchParams::DEFAULT_LIMIT)
        end
      end

      def add_path_to_group_watcher(path, watcher)
        return nil if @group.nil?
        group_watcher = find_group_from_metadata(path)
        group_watcher.add(path, watcher)
        group_watcher
      end

      def construct_group_key(named_captures)
        match_rule = []
        @group_keys.each { |key|
          match_rule.append(named_captures.fetch(key, GroupWatchParams::DEFAULT_KEY))
        }
        match_rule = match_rule.join(GroupWatchParams::REGEXP_JOIN)

        match_rule
      end

      def construct_groupwatchers
        @group.rule.each { |rule|
          match_rule = construct_group_key(rule.match)
          @group_watchers[match_rule] ||= GroupWatcher.new(@group.rate_period, rule.limit)
        }
      end

      def find_group(metadata)
        metadata_key = construct_group_key(metadata)
        gw_key = @group_watchers.keys.find { |regexp| metadata_key.match?(regexp) && regexp != @default_group_key }
        gw_key ||= @default_group_key

        @group_watchers[gw_key]
      end

      def find_group_from_metadata(path)
        begin
          metadata = @group.pattern.match(path).named_captures
          group_watcher = find_group(metadata)
        rescue
          log.warn "Cannot find group from metadata, Adding file in the default group"
          group_watcher = @group_watchers[@default_group_key]
        end

        group_watcher
      end
    end

    class GroupWatcher
      attr_accessor :current_paths, :limit, :rate_period
      attr_reader :number_lines_read, :round_index, :round_started_at

      # Per-file state for the current rate period (a "round").
      #
      # number_lines_read: lines delivered from this path in the current round
      # start_reading_time: when the path was first read in the current round
      # watchers: TailWatchers registered for this path
      # active: whether the path still has pending data. A reader is active on
      #         every notification and turns idle when it reaches EOF, so files
      #         without pending data surrender their share for reuse.
      # quota: lines this path may consume in the current round. Shares are
      #        granted up front to every registered reader, which keeps each
      #        reader bounded even if another reader drains its share first.
      FileCounter = Struct.new(
        :number_lines_read,
        :start_reading_time,
        :watchers,
        :active,
        :quota,
      ) do
        def active?
          active
        end

        def unused_quota
          [quota - number_lines_read, 0].max
        end
      end

      def initialize(rate_period = 60, limit = -1)
        @current_paths = {}
        @rate_period = rate_period
        @limit = limit
        @number_lines_read = 0
        @round_index = 0
        @round_started_at = nil
        @round_cursor = 0
      end

      def add(path, watcher)
        counter = @current_paths[path] ||= FileCounter.new(0, nil, Set.new, true, 0)
        counter.watchers << watcher
        # A round may already be running when a path is discovered. Grant its
        # share immediately instead of waiting for the next period.
        grant_share(counter) if @round_started_at
      end

      def include?(path)
        @current_paths.key?(path)
      end

      def size
        @current_paths.size
      end

      def delete(path, watcher)
        counter = @current_paths[path]
        return unless counter
        counter.watchers.delete(watcher)
        return unless counter.watchers.empty?

        @current_paths.delete(path)
        # Reuse the removed path's share without touching another round's budget.
        if @round_started_at && counter.active? && @limit >= 0
          redistribute(counter.unused_quota)
        end
      end

      def update_reading_time(path)
        @current_paths[path].start_reading_time ||= Fluent::Clock.now
      end

      def update_lines_read(path, value)
        counter = @current_paths[path]
        counter.number_lines_read += value
        @number_lines_read += value
      end

      # Called for every read notification. The up-front share is retained, so a
      # path that receives data while idle can read again as long as its share
      # was not fully reused by other readers. A fully reused share cannot be
      # granted again without taking the next round's budget, so that reader
      # waits for the next round instead.
      def mark_active(path)
        return unless include?(path)

        @current_paths[path].active = true
      end

      # Called when a reader reaches EOF. Its remaining share becomes reusable
      # by the other active readers within the same round.
      def mark_idle(path)
        return unless include?(path)

        counter = @current_paths[path]
        return unless counter.active?

        counter.active = false
        if @round_started_at && @limit >= 0
          reusable = counter.unused_quota
          # Shrink the quota to what was actually consumed so that a later
          # reactivation cannot claim the same lines twice.
          counter.quota = counter.number_lines_read
          redistribute(reusable)
        end
      end

      def active?(path)
        include?(path) && @current_paths[path].active?
      end

      def limit_lines_reached?(path)
        return true unless include?(path)
        return true if @limit == 0
        return false if @limit < 0

        roll_round if round_expired?

        counter = @current_paths[path]
        if counter.quota <= 0
          # Readers without a share (e.g. paths that join a fully granted round)
          # wait for the next round instead of borrowing its budget.
          true
        elsif counter.number_lines_read >= counter.quota
          true
        else
          false
        end
      end

      def to_s
        super + " current_paths: #{@current_paths} rate_period: #{@rate_period} limit: #{@limit}"
      end

      private

      # Paths ordered from the round's rotation cursor so that the remainder of
      # an indivisible limit is handed to a different reader every round.
      def ordered_counters
        counters = @current_paths.values
        return counters if counters.empty?

        offset = @round_cursor % counters.size
        counters[offset..] + counters[0...offset]
      end

      def active_counters
        ordered_counters.select(&:active?)
      end

      def round_expired?
        @round_started_at.nil? || Fluent::Clock.now - @round_started_at >= @rate_period
      end

      def roll_round
        @round_index += 1
        @round_cursor = (@round_cursor + 1) % [@current_paths.size, 1].max unless @round_started_at.nil?
        @round_started_at = Fluent::Clock.now
        @number_lines_read = 0
        @current_paths.each_value do |counter|
          counter.number_lines_read = 0
          counter.start_reading_time = nil
          counter.quota = 0
          # Every reader starts the new round with a fresh up-front share.
          counter.active = true
        end
        grant_initial_quotas
      end

      # Bounded allocation across every reader in the group. The shares are
      # granted up front when the round begins so that a reader cannot consume
      # another reader's share no matter when it gets scheduled. Every reader
      # is guaranteed at least one line; with more readers than the limit, the
      # group limit is exceeded by at most the number of readers, matching the
      # previous "one line per path" behavior instead of starving readers.
      def grant_initial_quotas
        readers = ordered_counters
        return if readers.empty?

        base = @limit / readers.size
        remainder = @limit - base * readers.size
        readers.each_with_index do |counter, index|
          counter.quota = base + (index < remainder ? 1 : 0)
          # A positive limit always lets each path deliver at least one line.
          counter.quota = 1 if counter.quota.zero?
        end
      end

      # Grant a share to a reader that joins a running round, using only budget
      # that was never handed out. Nothing is taken away from readers that
      # already hold a quota, and the next round is never touched. With no
      # ungranted budget left the reader waits for the next round.
      def grant_share(counter)
        return if @limit < 0
        return unless counter.quota <= 0

        granted = @current_paths.values.sum(&:quota)
        waiting = @current_paths.values.count { |candidate| candidate.quota <= 0 }
        return if waiting <= 0

        available = [@limit - granted, 0].max
        counter.quota = (available.to_f / waiting).floor
      end

      # Hand an idle reader's unused lines to the remaining active readers that
      # still have capacity, in round order.
      def redistribute(reusable)
        return if reusable <= 0

        active_counters.each do |counter|
          break if reusable <= 0

          remaining = counter.unused_quota
          next if remaining <= 0

          granted = [reusable, remaining].min
          counter.quota += granted
          reusable -= granted
        end
      end
    end
  end
end
