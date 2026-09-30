# frozen_string_literal: true
require_relative "spec_helper"

describe "statement_budget extension" do
  before do
    @db = Sequel.mock(:extensions=>[:statement_budget])
    @ds = @db[:items]
  end

  it "should allow statements up to the budget and commit" do
    @db.transaction(:statement_budget=>3) do
      @db.run("S1")
      @db.run("S2")
      @ds.all
    end

    @db.sqls.must_equal ["BEGIN", "S1", "S2", "SELECT * FROM items", "COMMIT"]
  end

  it "should not count transaction control or savepoint statements against the budget" do
    @db.transaction(:statement_budget=>1) do
      @db.transaction(:savepoint=>true) do
        @db.run("S1")
      end
    end

    @db.sqls.must_equal ["BEGIN", "SAVEPOINT autopoint_1", "S1", "RELEASE SAVEPOINT autopoint_1", "COMMIT"]
  end

  it "should reject the statement exceeding the budget without sending it to the adapter and roll back" do
    raised = nil
    begin
      @db.transaction(:statement_budget=>2) do
        @db.run("S1")
        @db.run("S2")
        @db.run("S3")
        flunk "should not reach"
      end
    rescue Sequel::StatementBudget::StatementBudgetExceeded => e
      raised = e
    end

    raised.is_a?(Sequel::StatementBudget::StatementBudgetExceeded).must_equal true
    raised.statement_budget.must_equal 2
    raised.statements_used.must_equal 2
    raised.trigger_location.must_be_kind_of String
    # S3 must never reach the adapter, but ROLLBACK must still run
    @db.sqls.must_equal ["BEGIN", "S1", "S2", "ROLLBACK"]
  end

  it "should reject the first business statement with a zero budget while still rolling back" do
    proc do
      @db.transaction(:statement_budget=>0) do
        @db.run("S1")
      end
    end.must_raise Sequel::StatementBudget::StatementBudgetExceeded

    @db.sqls.must_equal ["BEGIN", "ROLLBACK"]
  end

  it "should raise Error for an invalid budget option" do
    proc{@db.transaction(:statement_budget=>-1){}}.must_raise Sequel::Error
    proc{@db.transaction(:statement_budget=>"2"){}}.must_raise Sequel::Error
    proc{@db.transaction(:statement_budget=>1.5){}}.must_raise Sequel::Error
  end

  it "should share the remaining budget with nested transactions and savepoints" do
    e = nil
    begin
      @db.transaction(:statement_budget=>3) do
        @db.run("S1")
        @db.transaction(:savepoint=>true) do
          @db.run("S2")
          @db.run("S3")
          @db.run("S4")
        end
      end
      flunk "should not reach"
    rescue Sequel::StatementBudget::StatementBudgetExceeded => ex
      e = ex
    end

    e.statement_budget.must_equal 3
    e.statements_used.must_equal 3
    @db.sqls.must_equal ["BEGIN", "S1", "SAVEPOINT autopoint_1", "S2", "S3", "ROLLBACK TO SAVEPOINT autopoint_1", "ROLLBACK"]
  end

  it "should not give a nested transaction its own budget" do
    begin
      @db.transaction(:statement_budget=>1) do
        @db.run("S1")
        # Nested transaction claiming a fresh, larger budget must not get one.
        @db.transaction(:savepoint=>true, :statement_budget=>100) do
          @db.run("S2")
        end
      end
      flunk "should not reach"
    rescue Sequel::StatementBudget::StatementBudgetExceeded => e
      e.statement_budget.must_equal 1
      e.statements_used.must_equal 1
    end

    @db.sqls.must_equal ["BEGIN", "S1", "SAVEPOINT autopoint_1", "ROLLBACK TO SAVEPOINT autopoint_1", "ROLLBACK"]
  end

  it "should share the budget with statements from internal model callbacks" do
    @db.fetch = {:id=>1, :name=>"thing"}
    model = Class.new(Sequel::Model(@db[:items]))
    model.columns :id, :name
    @db.sqls
    model.send(:define_method, :after_create) do
      model.db.run("INSERT INTO audit VALUES (1)")
    end

    proc do
      @db.transaction(:statement_budget=>1) do
        model.create(:name=>"thing")
      end
    end.must_raise Sequel::StatementBudget::StatementBudgetExceeded

    # Model INSERT consumed the only statement, the callback statement is rejected,
    # and neither transaction gets a fresh budget.
    @db.sqls.must_equal ["BEGIN", "INSERT INTO items (name) VALUES ('thing')", "ROLLBACK"]
  end

  it "should still allow statements from internal model callbacks within the shared budget" do
    @db.autoid = 1
    @db.fetch = {:id=>1, :name=>"thing"}
    model = Class.new(Sequel::Model(@db[:items]))
    model.columns :id, :name
    @db.sqls
    model.send(:define_method, :after_create) do
      model.db.run("INSERT INTO audit VALUES (1)")
    end

    @db.transaction(:statement_budget=>3) do
      model.create(:name=>"thing")
    end

    @db.sqls.must_equal ["BEGIN", "INSERT INTO items (name) VALUES ('thing')", "SELECT * FROM items WHERE (id = 1) LIMIT 1", "INSERT INTO audit VALUES (1)", "COMMIT"]
  end

  it "should roll back to a savepoint after exhaustion and keep count shared" do
    begin
      @db.transaction(:statement_budget=>2) do
        @db.run("S1")
        @db.transaction(:savepoint=>true) do
          @db.run("S2")
          @db.run("S3")
        end
      end
    rescue Sequel::StatementBudget::StatementBudgetExceeded
    end

    @db.sqls.must_equal ["BEGIN", "S1", "SAVEPOINT autopoint_1", "S2", "ROLLBACK TO SAVEPOINT autopoint_1", "ROLLBACK"]

    # Savepoint rollback cleaned up, but the outer budget stays exhausted.
    proc do
      @db.transaction(:statement_budget=>2) do
        @db.run("S1")
        begin
          @db.transaction(:savepoint=>true) do
            @db.run("S2")
            @db.run("S3")
          end
        rescue Sequel::StatementBudget::StatementBudgetExceeded
        end
        @db.run("S4")
      end
    end.must_raise Sequel::StatementBudget::StatementBudgetExceeded
  end

  it "should not record SQL or bind argument values in the exception" do
    begin
      @db.transaction(:statement_budget=>0) do
        @ds.where(:name=>"super_secret_value").all
      end
      flunk "should not reach"
    rescue Sequel::StatementBudget::StatementBudgetExceeded => e
      e.message.wont_include "super_secret_value"
      e.message.wont_include "SELECT"
      e.inspect.wont_include "super_secret_value"
      e.trigger_location.must_be_kind_of String
    end
  end

  it "should not record prepared statement bind argument values in the exception" do
    begin
      @db.transaction(:statement_budget=>0) do
        ps = @ds.where(:name=>"p@ssw0rd!").prepare(:first, :secret_lookup)
        @db.call(:secret_lookup)
      end
      flunk "should not reach"
    rescue Sequel::StatementBudget::StatementBudgetExceeded => e
      e.message.wont_include "p@ssw0rd!"
      e.inspect.wont_include "p@ssw0rd!"
    end
  end

  it "should report the application trigger location" do
    trigger_line = nil
    begin
      @db.transaction(:statement_budget=>0) do
        trigger_line = __LINE__ + 1
        @db.run("S1")
      end
      flunk "should not reach"
    rescue Sequel::StatementBudget::StatementBudgetExceeded => e
      e.trigger_location.must_include __FILE__
      e.trigger_location.must_include trigger_line.to_s
    end
  end

  it "should not apply a budget outside of budgeted transactions" do
    @db.transaction do
      @db.run("S1")
      @db.run("S2")
    end
    @db.run("S3")
    @db.sqls.must_equal ["BEGIN", "S1", "S2", "COMMIT", "S3"]
  end

  it "should run after_rollback hooks after budget exhaustion" do
    hook_ran = false
    begin
      @db.transaction(:statement_budget=>0) do
        @db.after_rollback{hook_ran = true}
        @db.run("S1")
      end
    rescue Sequel::StatementBudget::StatementBudgetExceeded
    end
    hook_ran.must_equal true
  end

  it "should allow manual Sequel::Rollback after budget exhaustion" do
    proc do
      @db.transaction(:statement_budget=>1, :rollback=>:reraise) do
        @db.run("S1")
        begin
          @db.run("S2")
        rescue Sequel::StatementBudget::StatementBudgetExceeded
        end
        raise Sequel::Rollback
      end
    end.must_raise Sequel::Rollback

    @db.sqls.must_equal ["BEGIN", "S1", "ROLLBACK"]
  end
end
