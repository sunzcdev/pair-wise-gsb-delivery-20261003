#!/usr/bin/env ruby
# frozen_string_literal: true

# Offline recomputation of an actor-percentage rollout across a grouping-salt
# change in Flipper. In Flipper the "salt" of percentage-of-actors bucketing is
# the feature name itself. Both code paths hash identically:
#
#   lib/flipper/gates/percentage_of_actors.rb
#   lib/flipper/expressions/percentage_of_actors.rb
#
#   Zlib.crc32("#{feature_name}#{actor_id}") % 100_000 < percentage * 1_000
#
# Renaming / re-salting therefore reshuffles every actor. This script proves,
# from the actual hit sets on a fixed actor population, that the transition
# plan below never removes an actor:
#
#   * the old-salt threshold is FROZEN at its current 10% for the whole window;
#   * the new-salt rule is declared at 0 and only ever rises (monotonic);
#   * the effective gate is the UNION (Any/OR) of both conventions.
#
# Membership at every stage is a pure function of (salt, actor_id, threshold).
# Nothing per actor is ever stored, so the run is fully reproducible offline
# (stdlib only) and can be re-executed in CI as a gate.

require "zlib"
require "set"

module SaltMigration
  SCALING_FACTOR = 1_000
  BUCKETS = 100 * SCALING_FACTOR

  # The salt is the feature name. Picking a new feature name is exactly what
  # produces a new independent bucketing.
  OLD_SALT = "search"
  NEW_SALT = "search_v2"

  # Fixed, deterministic actor population. Override size with ARGV[0].
  ACTOR_IDS = (1..(ARGV[0] || 5_000).to_i).map { |i| "User;#{i}" }.freeze

  # Declarative transition plan. nil means the rule is absent.
  # The old percentage never moves until the new salt already covers everyone.
  PLAN = [
    ["0. pre-switch: old salt only (10%)",       10, nil],
    ["1. first switch: declare new rule at 0%",  10, 0],
    ["2. widen new threshold to 2%",             10, 2],
    ["3. widen new threshold to 5%",             10, 5],
    ["4. new threshold reaches parity (10%)",    10, 10],
    ["5. widen new threshold to 25%",            10, 25],
    ["6. widen new threshold to 50%",            10, 50],
    ["7. new salt covers 100%",                  10, 100],
    ["8. end of transition: retire old salt",    nil, 100],
  ].freeze

  module_function

  # Exact replica of Flipper's bucketing. Keep in sync with the two files above.
  def bucket(salt, actor_id)
    Zlib.crc32("#{salt}#{actor_id}") % BUCKETS
  end

  def member?(salt, percentage, actor_id)
    return false if percentage.nil? || percentage <= 0
    return true if percentage >= 100
    bucket(salt, actor_id) < percentage * SCALING_FACTOR
  end

  def old_member?(percentage, actor_id) = member?(OLD_SALT, percentage, actor_id)
  def new_member?(percentage, actor_id) = member?(NEW_SALT, percentage, actor_id)

  def hit_set(old_pct, new_pct)
    ACTOR_IDS.each_with_object(Set.new) do |id, hits|
      hits << id if old_member?(old_pct, id) || new_member?(new_pct, id)
    end
  end

  def reason_counts(old_pct, new_pct)
    counts = Hash.new(0)
    ACTOR_IDS.each do |id|
      o = old_member?(old_pct, id)
      n = new_member?(new_pct, id)
      if o && n then counts[:both] += 1
      elsif o then counts[:old_only] += 1
      elsif n then counts[:new_only] += 1
      else counts[:off] += 1
      end
    end
    counts
  end

  def run
    failures = []
    puts "== Flipper salt-rollout offline recomputation =="
    puts "actors: #{ACTOR_IDS.size}  old salt: #{OLD_SALT.inspect}  new salt: #{NEW_SALT.inspect}"
    puts "formula: Zlib.crc32(\"\#{salt}\#{actor_id}\") % #{BUCKETS} < pct * #{SCALING_FACTOR}"
    puts

    puts "== Per-actor buckets (first 8 actors; threshold 10% => bucket < 10_000) =="
    puts "%-10<id>s %10<old_bucket>s %-8<old_hit>s %10<new_bucket>s %-8<new_hit>s" %
      { id: "actor", old_bucket: "old bucket", old_hit: "old 10%", new_bucket: "new bucket", new_hit: "new 10%" }
    ACTOR_IDS.first(8).each do |id|
      ob = bucket(OLD_SALT, id)
      nb = bucket(NEW_SALT, id)
      puts "%-10<id>s %10<ob>d %-8<oh>s %10<nb>d %-8<nh>s" %
        { id: id, ob: ob, oh: (ob < 10_000).to_s, nb: nb, nh: (nb < 10_000).to_s }
    end
    puts

    # What a naive cut-over (swap the salt while keeping 10%) would have done.
    old_10 = Set.new(ACTOR_IDS.select { |id| old_member?(10, id) })
    new_10 = Set.new(ACTOR_IDS.select { |id| new_member?(10, id) })
    dropped = old_10 - new_10
    puts "== Naive cut-over comparison (salt swapped directly at 10%) =="
    puts "old-salt 10% cohort: #{old_10.size} actors"
    puts "new-salt 10% cohort: #{new_10.size} actors"
    puts "overlap (kept anyway): #{(old_10 & new_10).size} actors"
    puts "would EXIT unexpectedly under a naive swap: #{dropped.size} actors " \
         "(#{(100.0 * dropped.size / ACTOR_IDS.size).round(2)}% of population)"
    puts "examples that would exit: #{dropped.to_a.first(5).inspect}"
    puts "=> the union plan below keeps every one of these via the frozen old rule"
    puts

    puts "== Transition plan driven against the hash =="
    puts "%-44<stage>s %5<old>s %5<new>s %7<hits>s %7<added>s %7<exited>s  reasons" %
      { stage: "stage", old: "old%", new: "new%", hits: "hits", added: "added", exited: "exited" }

    previous = nil
    PLAN.each do |label, old_pct, new_pct|
      hits = hit_set(old_pct, new_pct)
      added = previous ? (hits - previous).size : 0
      exited = previous ? (previous - hits).size : 0
      failures << "#{label}: #{exited} actors exited" if exited != 0
      rc = reason_counts(old_pct, new_pct)
      puts "%-44<stage>s %5<old>s %5<new>s %7<hits>d %7<added>d %7<exited>d  " \
           "old-only=%<oo>d both=%<both>d new-only=%<no>d off=%<off>d" % {
        stage: label,
        old: old_pct.inspect,
        new: new_pct.inspect,
        hits: hits.size,
        added: added,
        exited: exited,
        oo: rc[:old_only],
        both: rc[:both],
        no: rc[:new_only],
        off: rc[:off],
      }
      previous = hits
    end
    puts

    final = hit_set(nil, 100)
    unless final.size == ACTOR_IDS.size
      failures << "final coverage was #{final.size}/#{ACTOR_IDS.size}, expected everyone"
    end

    # Invariant that makes "no surprise exit" an actual proof: at every stage
    # the effective set is S(old_pct) UNION S(new_pct), old_pct is frozen while
    # new_pct only rises, so both operands are monotonic and the union is too.
    monotonic = PLAN.each_cons(2).all? do |(_, o1, n1), (_, o2, n2)|
      (o1.nil? || o2 == o1 || o2.nil?) && (n1.nil? || n2.nil? || n2 >= n1)
    end
    failures << "plan is not monotonic (old frozen, new non-decreasing)" unless monotonic

    if failures.empty?
      puts "OK: zero actors exited at every stage; final set covers all #{ACTOR_IDS.size} actors."
      puts "OK: plan thresholds are monotonic (old salt frozen, new salt non-decreasing)."
      exit 0
    else
      warn "FAIL:"
      failures.each { |f| warn "  - #{f}" }
      exit 1
    end
  end
end

SaltMigration.run
