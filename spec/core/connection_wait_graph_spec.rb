# frozen_string_literal: true
require_relative "spec_helper"

# Simple reusable barrier so tests can ensure all threads hold their
# connections before any thread attempts to borrow another shard,
# without relying on sleeps.
class WaitGraphSpecBarrier
  def initialize(count)
    @count = count
    @arrived = 0
    @mutex = Mutex.new
    @cv = ConditionVariable.new
  end

  def wait
    @mutex.synchronize do
      @arrived += 1
      if @arrived >= @count
        @cv.broadcast
      else
        @cv.wait(@mutex) while @arrived < @count
      end
    end
  end
end

describe "Connection pool wait graph" do
  def wait_graph
    Sequel.connection_wait_graph
  end

  def wait_graph_db(pool_class, servers, opts={})
    Sequel.mock({
      :max_connections=>1,
      :pool_timeout=>5,
      :pool_wait_graph=>true,
      :pool_class=>pool_class,
      :servers=>servers,
      :password=>'secret1'
    }.merge(opts))
  end

  # Have one thread per server hold that server's only connection, then
  # borrow the next server's connection (cyclically).  Returns the
  # errors rescued by the threads.
  def run_wait_cycle(pool, servers)
    barrier = WaitGraphSpecBarrier.new(servers.length)
    errors = Queue.new
    threads = servers.each_with_index.map do |server, i|
      borrow = servers[(i + 1) % servers.length]
      Thread.new do
        pool.hold(server) do
          barrier.wait
          begin
            pool.hold(borrow){}
          rescue Sequel::PoolWaitCycle, Sequel::PoolTimeout => e
            errors << e
          end
        end
      end
    end
    threads.each{|t| t.join(30)}
    threads.each{|t| t.value}
    errors
  end

  def queued_errors(errors)
    a = []
    a << errors.pop until errors.empty?
    a
  end

  sharded_pool_classes = [:sharded_threaded]
  sharded_pool_classes << :sharded_timed_queue if RUBY_VERSION >= '3.2'
  sharded_pool_classes.each do |pool_class|
    describe "with #{pool_class} pool" do
      it "should abort the borrow completing a two-owner wait cycle" do
        db = wait_graph_db(pool_class, {:s1=>{}, :s2=>{}})
        errors = queued_errors(run_wait_cycle(db.pool, [:s1, :s2]))
        cycles = errors.select{|e| e.is_a?(Sequel::PoolWaitCycle)}
        cycles.length.must_equal 1
        message = cycles.first.message
        message.must_include "wait cycle"
        message.must_include ":s1"
        message.must_include ":s2"
        message.wont_include "secret1"
        wait_graph.empty?.must_equal true
      end

      it "should abort the borrow completing a three-owner wait cycle" do
        db = wait_graph_db(pool_class, {:s1=>{}, :s2=>{}, :s3=>{}})
        errors = queued_errors(run_wait_cycle(db.pool, [:s1, :s2, :s3]))
        cycles = errors.select{|e| e.is_a?(Sequel::PoolWaitCycle)}
        cycles.length.must_equal 1
        message = cycles.first.message
        message.must_include ":s1"
        message.must_include ":s2"
        message.must_include ":s3"
        message.wont_include "secret1"
        wait_graph.empty?.must_equal true
      end

      it "should not raise for acyclic waits" do
        db = wait_graph_db(pool_class, {})
        pool = db.pool
        acquired = Queue.new
        started = Queue.new
        t2 = nil
        pool.hold(:default) do
          t2 = Thread.new do
            started << true
            pool.hold(:default){acquired << true}
          end
          started.pop
          100.times{break if wait_graph.waiting_on(t2); sleep 0.01}
          wait_graph.waiting_on(t2).wont_be_nil
          acquired.empty?.must_equal true
        end
        acquired.pop.must_equal true
        t2.join
        wait_graph.empty?.must_equal true
      end

      it "should update the graph after pool timeouts" do
        db = wait_graph_db(pool_class, {}, :pool_timeout=>0.2)
        pool = db.pool
        error = nil
        pool.hold(:default) do
          t2 = Thread.new do
            begin
              pool.hold(:default){}
            rescue Sequel::PoolTimeout => e
              error = e
            end
          end
          t2.join
        end
        error.must_be_instance_of Sequel::PoolTimeout
        wait_graph.empty?.must_equal true
      end

      it "should allow reentrant holds by the same owner" do
        db = wait_graph_db(pool_class, {})
        pool = db.pool
        pool.hold(:default) do |c1|
          wait_graph.empty?.must_equal false
          pool.hold(:default) do |c2|
            c2.must_equal c1
            wait_graph.waiting_on(Thread.current).must_be_nil
          end
        end
        wait_graph.empty?.must_equal true
      end

      it "should update the graph when the block raises" do
        db = wait_graph_db(pool_class, {})
        pool = db.pool
        proc{pool.hold(:default){raise ArgumentError}}.must_raise ArgumentError
        wait_graph.empty?.must_equal true
      end

      it "should not detect cycles when the option is not enabled" do
        db = wait_graph_db(pool_class, {:s1=>{}, :s2=>{}}, :pool_wait_graph=>nil, :pool_timeout=>0.2)
        errors = queued_errors(run_wait_cycle(db.pool, [:s1, :s2]))
        errors.wont_be_empty
        errors.each{|e| e.must_be_instance_of Sequel::PoolTimeout}
        wait_graph.empty?.must_equal true
      end
    end
  end

  pool_classes = [:threaded]
  pool_classes << :timed_queue if RUBY_VERSION >= '3.2'
  pool_classes.each do |pool_class|
    it "should detect wait cycles spanning multiple #{pool_class} pools" do
      db1 = wait_graph_db(pool_class, {})
      db2 = wait_graph_db(pool_class, {})
      barrier = WaitGraphSpecBarrier.new(2)
      errors = Queue.new
      threads = [
        Thread.new do
          db1.pool.hold do
            barrier.wait
            begin
              db2.pool.hold{}
            rescue Sequel::PoolWaitCycle => e
              errors << e
            end
          end
        end,
        Thread.new do
          db2.pool.hold do
            barrier.wait
            begin
              db1.pool.hold{}
            rescue Sequel::PoolWaitCycle => e
              errors << e
            end
          end
        end
      ]
      threads.each{|t| t.join(30)}
      threads.each{|t| t.value}
      cycles = queued_errors(errors)
      cycles.length.must_equal 1
      cycles.first.message.must_include "wait cycle"
      wait_graph.empty?.must_equal true
    end
  end
end
