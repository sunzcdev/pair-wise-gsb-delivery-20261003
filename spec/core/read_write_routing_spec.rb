# frozen-string-literal: true
require_relative "spec_helper"

# These specs verify the read/write routing policy using local connection
# doubles that carry their identity (the server they were opened for).  No
# replication setup is required: the doubles record which connection each
# statement actually ran on, and the mock adapter records the server each
# query was sent to as a trailing "-- server" comment.

describe "Database read/write server routing" do
  # A connection double that knows which server it belongs to, and records
  # the SQL executed directly on it (e.g. BEGIN/COMMIT/SAVEPOINT).
  class RoutingTestConnection
    attr_reader :server, :sqls

    def initialize(server)
      @server = server
      @sqls = []
    end

    def execute(sql)
      @sqls << sql
      nil
    end
  end

  # Create a mock Database whose connections are identity-bearing doubles.
  # Returns the database and the array that will contain every connection
  # created, in creation order.  The :test=>false option is used so the
  # database does not create a connection before we install our doubles.
  def routing_database(opts={})
    connections = []
    db = Sequel.mock({:fetch=>{:id=>1}, :test=>false}.merge(opts))
    db.define_singleton_method(:connect) do |server|
      conn = RoutingTestConnection.new(server)
      connections << conn
      conn
    end
    [db, connections]
  end

  before do
    @db, @connections = routing_database(:servers=>{:read_only=>{}})
  end

  it "Database#read_only_sql? should only classify statements it can prove are reads" do
    @db.read_only_sql?('SELECT * FROM items').must_equal true
    @db.read_only_sql?('  /* leading comment */ select 1').must_equal true
    @db.read_only_sql?('WITH a AS (SELECT 1) SELECT * FROM a').must_equal true
    @db.read_only_sql?('VALUES (1), (2)').must_equal true

    # Data modification anywhere in the statement (including CTEs) is a write.
    @db.read_only_sql?('WITH d AS (DELETE FROM items RETURNING *) SELECT * FROM d').must_equal false
    @db.read_only_sql?('SELECT * INTO new_table FROM items').must_equal false
    @db.read_only_sql?('INSERT INTO items VALUES (1)').must_equal false
    @db.read_only_sql?('UPDATE items SET a = 1').must_equal false

    # Locking clauses are not read-only.
    @db.read_only_sql?('SELECT * FROM items FOR UPDATE').must_equal false
    @db.read_only_sql?('SELECT * FROM items LOCK IN SHARE MODE').must_equal false

    # Sequences are not read-only.
    @db.read_only_sql?("SELECT nextval('items_id_seq')").must_equal false

    # Multiple statements cannot be proven to be read-only.
    @db.read_only_sql?('SELECT 1; DELETE FROM items').must_equal false
    @db.read_only_sql?('SELECT 1;').must_equal false

    # Only inspecting the first word would be wrong: this starts with SELECT.
    @db.read_only_sql?('SELECT 1; DROP TABLE items').must_equal false
  end

  it "Database#read_only_sql? should ignore keywords inside comments, strings, and quoted identifiers" do
    @db.read_only_sql?("SELECT 'DELETE FROM items' AS s").must_equal true
    @db.read_only_sql?('SELECT "update" FROM t').must_equal true
    @db.read_only_sql?("SELECT * FROM items -- FOR UPDATE").must_equal true
    @db.read_only_sql?("SELECT * FROM items /* update items */").must_equal true
  end

  it "Database#read_only_sql? should not classify SQL it cannot safely tokenize" do
    # Unterminated string/comment means no classification, so not a confirmed read.
    @db.read_only_sql?("SELECT * FROM items WHERE a = 'unterminated").must_equal false
    @db.read_only_sql?('SELECT * FROM items /* unterminated').must_equal false

    # Not a string at all.
    @db.read_only_sql?(:select).must_equal false
  end

  it "Database#read_sql_server should follow the declared conservative policy by default" do
    @db.read_sql_server('SELECT * FROM items').must_equal :read_only
    @db.read_sql_server('UPDATE items SET a = 1').must_equal :default
    # Cannot be classified, so the conservative default applies.
    @db.read_sql_server('EXPLAIN ANALYZE DELETE FROM items').must_equal :default
    @db.read_sql_server('SOMETHING WE DO NOT UNDERSTAND').must_equal :default
  end

  it "Database#read_sql_server should follow an explicit :read_only policy for unclassifiable SQL" do
    @db.read_only_sql_policy = :read_only
    @db.read_sql_server('SELECT * FROM items').must_equal :read_only
    @db.read_sql_server('SOMETHING WE DO NOT UNDERSTAND').must_equal :read_only
    # Provable writes are still not sent to the replica.
    @db.read_sql_server('DELETE FROM items').must_equal :default
  end

  it "Database#read_sql_server should call a callable policy with the SQL string" do
    seen = []
    @db.read_only_sql_policy = proc{|sql| seen << sql; :read_only}
    @db.read_sql_server('SOMETHING WE DO NOT UNDERSTAND').must_equal :read_only
    seen.must_equal ['SOMETHING WE DO NOT UNDERSTAND']

    @db.read_only_sql_policy = proc{|sql| nil}
    @db.read_sql_server('SOMETHING WE DO NOT UNDERSTAND').must_equal :default
  end

  it "Database#read_only_sql_policy= should reject unsupported values" do
    proc{@db.read_only_sql_policy = :bogus}.must_raise Sequel::Error
  end

  it "reads inside a transaction should run on the transaction's write connection" do
    read_connection = nil
    txn_connection = nil

    @db.transaction do |conn|
      txn_connection = conn
      @db.synchronize(:read_only){|c| read_connection = c}
      @db.synchronize{|c| c.must_be_same_as conn}
      @db[:items].all
    end

    read_connection.must_be_same_as txn_connection
    txn_connection.server.must_equal :default
    txn_connection.sqls.must_equal ['BEGIN', 'COMMIT']

    # The SELECT inside the transaction ran on the write server, not the replica.
    @db.sqls.must_equal ['SELECT * FROM items']
    @connections.map(&:server).uniq.must_equal [:default]
  end

  it "savepoints should be created on the transaction's write connection, not the replica" do
    @db.transaction do
      @db.transaction(:savepoint=>true, :server=>:read_only) do
        @db[:items].all
      end
    end

    default = @connections.find{|c| c.server == :default}
    default.wont_be_nil
    default.sqls.must_equal ['BEGIN', 'SAVEPOINT autopoint_1', 'RELEASE SAVEPOINT autopoint_1', 'COMMIT']

    # Nothing should have run on a replica connection.
    @connections.select{|c| c.server == :read_only}.must_be_empty
  end

  it "nested synchronize inside a transaction should not yield a connection from another server" do
    outer = inner = nil
    @db.transaction do |conn|
      outer = conn
      @db.synchronize(:read_only) do |c|
        inner = c
        @db.synchronize(:read_only){|c2| c2.must_be_same_as conn}
        @db.synchronize{|c2| c2.must_be_same_as conn}
      end
    end
    inner.must_be_same_as outer

    # No separate :read_only connection was ever created.
    @connections.map(&:server).uniq.must_equal [:default]
  end

  it "failure inside a nested read should not release the transaction connection to the pool" do
    txn_connection = nil
    e = Class.new(StandardError)

    proc do
      @db.transaction do |conn|
        txn_connection = conn
        @db.synchronize(:read_only){raise e}
      end
    end.must_raise e

    # The transaction was rolled back on the transaction connection.
    txn_connection.sqls.must_equal ['BEGIN', 'ROLLBACK']

    # The connection was returned to the pool exactly once (by the transaction),
    # and is still usable by a later checkout.
    @db.pool.size(:default).must_equal 1
    later = nil
    @db.synchronize{|c| later = c}
    later.must_be_same_as txn_connection
  end

  it "should not let another thread borrow the transaction connection through the :read_only server" do
    db, = routing_database(:servers=>{:read_only=>{}}, :max_connections=>1, :pool_class=>:threaded)
    db.pool.instance_variable_set(:@timeout, 0.2)

    e = Class.new(StandardError)
    started = Queue.new
    resume = Queue.new
    txn_connection = nil

    t = Thread.new do
      begin
        db.transaction do |conn|
          txn_connection = conn
          # A nested read on the :read_only server must use the transaction
          # connection, never a connection borrowed from the replica.
          db.synchronize(:read_only){|c| raise 'not the transaction connection' unless c.equal?(conn)}
          started.push(nil)
          resume.pop
          db.synchronize(:read_only){raise e}
        end
      rescue e
      end
    end

    started.pop
    # While the transaction is holding the only connection, another thread
    # cannot get a :read_only connection.
    other = Thread.new do
      begin
        db.pool.hold(:read_only){|c| :got}
      rescue Sequel::PoolTimeout
        :timeout
      end
    end.value
    other.must_equal :timeout

    resume.push(nil)
    t.join

    # After the failure, the transaction connection is released exactly once and
    # is available for a later checkout.
    db.pool.size.must_equal 1
    db.pool.available_connections.must_equal [txn_connection]
  end

  it "a read requested for :read_only inside a transaction on another shard stays on the transaction connection" do
    db, connections = routing_database(:servers=>{:read_only=>{}, :shard1=>{}})

    read_connection = nil
    txn_connection = nil
    db.transaction(:server=>:shard1) do |conn|
      txn_connection = conn
      db.synchronize(:read_only){|c| read_connection = c}
    end
    read_connection.must_be_same_as txn_connection
    read_connection.server.must_equal :shard1

    # No replica connection was borrowed while the transaction was active.
    connections.map(&:server).uniq.must_equal [:shard1]
  end

  it "should not pin reads when the :read_only server is not separately configured" do
    db, = routing_database(:servers=>{:shard1=>{}})

    found = nil
    db.transaction(:server=>:shard1) do
      db.synchronize(:read_only){|c| found = c}
    end
    # :read_only resolves to the :default server because no replica is declared.
    found.server.must_equal :default
  end
end

describe "Dataset read/write server routing" do
  before do
    @db = Sequel.mock(:servers=>{:read_only=>{}}, :fetch=>{:id=>1})
  end

  it "Sequel-generated SELECTs use the replica outside a transaction" do
    @db[:items].all
    @db.sqls.must_equal ['SELECT * FROM items -- read_only']
  end

  it "Sequel-generated SELECTs use the write connection inside a transaction" do
    @db.transaction{@db[:items].all}
    @db.sqls.must_equal ['BEGIN', 'SELECT * FROM items', 'COMMIT']
  end

  it "raw SQL from Dataset#with_sql uses the replica only when it is provably read-only" do
    @db[:items].with_sql('SELECT * FROM items').all
    @db.sqls.must_equal ['SELECT * FROM items -- read_only']

    @db[:items].with_sql('UPDATE items SET a = 1').all
    @db.sqls.must_equal ['UPDATE items SET a = 1']

    @db[:items].with_sql('SOMETHING WE DO NOT UNDERSTAND').all
    @db.sqls.must_equal ['SOMETHING WE DO NOT UNDERSTAND']
  end

  it "raw SQL from Database#fetch follows the same classification" do
    @db.fetch('SELECT * FROM items').all
    @db.sqls.must_equal ['SELECT * FROM items -- read_only']

    @db.fetch('DELETE FROM items').all
    @db.sqls.must_equal ['DELETE FROM items']
  end

  it "raw SQL with placeholders is classified by its statement text, not its arguments" do
    # A parameterized SELECT is still a read; the argument never changes that.
    @db.fetch('SELECT * FROM items WHERE a = ?', 1).all
    @db.sqls.must_equal ["SELECT * FROM items WHERE a = 1 -- read_only"]

    # The first word alone is not enough: modification appears after it, so
    # the statement is conservatively sent to the write server.
    @db.fetch('SELECT * FROM items; DELETE FROM items WHERE a = ?', 1).all
    @db.sqls.must_equal ["SELECT * FROM items; DELETE FROM items WHERE a = 1"]
  end

  it "raw SQL classified as read-only under an explicit policy uses the replica" do
    @db.read_only_sql_policy = :read_only
    @db[:items].with_sql('SOMETHING WE DO NOT UNDERSTAND').all
    @db.sqls.must_equal ['SOMETHING WE DO NOT UNDERSTAND -- read_only']
  end

  it "an explicit Dataset#server is always honored" do
    @db[:items].server(:read_only).delete
    @db.sqls.must_equal ['DELETE FROM items -- read_only']
    @db[:items].server(:default).all
    @db.sqls.must_equal ['SELECT * FROM items']
  end
end
