# frozen-string-literal: true

module Sequel
  class Database
    # ---------------------
    # :section: 4 - Methods relating to adapters, connecting, disconnecting, and sharding
    # This methods involve the Database's connection pool.
    # ---------------------

    # Array of supported database adapters
    ADAPTERS = %w'ado amalgalite ibmdb jdbc mock mysql mysql2 odbc oracle postgres sqlanywhere sqlite tinytds trilogy'.map(&:to_sym)

    # Keywords that start statements that always write to the database (or
    # change session/server state), and therefore must not be sent to a
    # read-only replica.
    READ_WRITE_SQL_KEYWORDS = %w'insert update delete merge upsert replace set reset grant revoke
      create alter drop truncate rename comment vacuum analyze attach detach lock unlock
      copy call do execute exec prepare deallocate begin start commit rollback savepoint release
      listen unlisten notify load import use declare fetch move close discard refresh cluster
      security check reindex'.freeze

    # Keywords that, when present anywhere in an otherwise read-only statement,
    # mean the statement cannot be treated as read-only.  This catches data
    # modification inside CTEs, SELECT ... FOR UPDATE/SHARE, SELECT INTO,
    # and sequence advancement, instead of only inspecting the first word.
    READ_ONLY_SQL_UNSAFE_KEYWORDS = %w'insert update delete merge upsert replace create
      alter drop truncate rename grant revoke into for lock call do execute exec copy
      sequence nextval setval currval analyze vacuum attach detach
      prepare deallocate listen notify load import'.freeze

    # Statements that are only safe to consider read-only if they are reading
    # a query that itself is read-only (e.g. EXPLAIN SELECT ...).  A statement
    # starting with one of these keywords is classified as :unknown by default.
    READ_ONLY_SQL_EXPLAIN_KEYWORDS = %w'show describe desc explain'.freeze

    # The keywords that can start a statement that only reads from the database.
    READ_ONLY_SQL_LEADING_KEYWORDS = %w'select with values table'.freeze

    # The Database subclass for the given adapter scheme.
    # Raises Sequel::AdapterNotFound if the adapter
    # could not be loaded.
    def self.adapter_class(scheme)
      scheme.is_a?(Class) ? scheme : load_adapter(scheme.to_sym)
    end

    # Returns the scheme symbol for the Database class.
    def self.adapter_scheme
      @scheme
    end
    
    # Connects to a database.  See Sequel.connect.
    def self.connect(conn_string, opts = OPTS)
      case conn_string
      when String
        if conn_string.start_with?('jdbc:')
          c = adapter_class(:jdbc)
          opts = opts.merge(:orig_opts=>opts.dup)
          opts = {:uri=>conn_string}.merge!(opts)
        else
          uri = URI.parse(conn_string)
          scheme = uri.scheme
          c = adapter_class(scheme)
          opts = c.send(:options_from_uri, uri).merge!(opts).merge!(:orig_opts=>opts.dup, :uri=>conn_string, :adapter=>scheme)
        end
      when Hash
        opts = conn_string.merge(opts)
        opts = opts.merge(:orig_opts=>opts.dup)
        c = adapter_class(opts[:adapter_class] || opts[:adapter] || opts['adapter'])
      else
        raise Error, "Sequel::Database.connect takes either a Hash or a String, given: #{conn_string.inspect}"
      end

      opts = opts.inject({}) do |m, (k,v)|
        k = :user if k.to_s == 'username'
        m[k.to_sym] = v
        m
      end

      begin
        db = c.new(opts)
        if defined?(yield)
          return yield(db)
        end
      ensure
        if defined?(yield)
          db.disconnect if db
          Sequel.synchronize{::Sequel::DATABASES.delete(db)}
        end
      end
      db
    end
    
    # Load the adapter from the file system.  Raises Sequel::AdapterNotFound
    # if the adapter cannot be loaded, or if the adapter isn't registered
    # correctly after being loaded. Options:
    # :map :: The Hash in which to look for an already loaded adapter (defaults to ADAPTER_MAP).
    # :subdir :: The subdirectory of sequel/adapters to look in, only to be used for loading
    #            subadapters.
    def self.load_adapter(scheme, opts=OPTS)
      map = opts[:map] || ADAPTER_MAP
      if subdir = opts[:subdir]
        file = "#{subdir}/#{scheme}"
      else
        file = scheme
      end
      
      unless obj = Sequel.synchronize{map[scheme]}
        # attempt to load the adapter file
        begin
          require "sequel/adapters/#{file}"
        rescue LoadError => e
          # If subadapter file doesn't exist, just return, 
          # using the main adapter class without database customizations.
          return if subdir
          raise Sequel.convert_exception_class(e, AdapterNotFound)
        end
        
        # make sure we actually loaded the adapter
        unless obj = Sequel.synchronize{map[scheme]}
          raise AdapterNotFound, "Could not load #{file} adapter: adapter class not registered in ADAPTER_MAP"
        end
      end

      obj
    end

    # Sets the adapter scheme for the Database class. Call this method in
    # descendants of Database to allow connection using a URL. For example the
    # following:
    #
    #   class Sequel::MyDB::Database < Sequel::Database
    #     set_adapter_scheme :mydb
    #     ...
    #   end
    #
    # would allow connection using:
    #
    #   Sequel.connect('mydb://user:password@dbserver/mydb')
    def self.set_adapter_scheme(scheme) # :nodoc:
      @scheme = scheme
      Sequel.synchronize{ADAPTER_MAP[scheme] = self}
    end
    private_class_method :set_adapter_scheme

    # Sets the given module as the shared adapter module for the given scheme.
    # Used to register shared adapters for use by the mock adapter. Example:
    #
    #   # in file sequel/adapters/shared/mydb.rb
    #   module Sequel::MyDB
    #     Sequel::Database.set_shared_adapter_scheme :mydb, self
    #
    #     def self.mock_adapter_setup(db)
    #       # ...
    #     end
    #
    #     module DatabaseMethods
    #       # ...
    #     end
    #
    #     module DatasetMethods
    #       # ...
    #     end
    #   end
    #
    # would allow the mock adapter to return a Database instance that supports
    # the MyDB syntax via:
    #
    #   Sequel.connect('mock://mydb')
    def self.set_shared_adapter_scheme(scheme, mod)
      Sequel.synchronize{SHARED_ADAPTER_MAP[scheme] = mod}
    end
    
    # The connection pool for this Database instance.  All Database instances have
    # their own connection pools.
    attr_reader :pool

    # Returns the scheme symbol for this instance's class, which reflects which
    # adapter is being used.  In some cases, this can be the same as the
    # +database_type+ (for native adapters), in others (i.e. adapters with
    # subadapters), it will be different.
    #
    #   Sequel.connect('jdbc:postgres://...').adapter_scheme
    #   # => :jdbc
    def adapter_scheme
      self.class.adapter_scheme
    end

    # Dynamically add new servers or modify server options at runtime. Also adds new
    # servers to the connection pool. Only usable when using a sharded connection pool.
    #
    # servers argument should be a hash with server name symbol keys and hash or
    # proc values.  If a servers key is already in use, it's value is overridden
    # with the value provided.
    #
    #   DB.add_servers(f: {host: "hash_host_f"})
    def add_servers(servers)
      unless sharded?
        raise Error, "cannot call Database#add_servers on a Database instance that does not use a sharded connection pool"
      end

      h = @opts[:servers]
      Sequel.synchronize{h.merge!(servers)}
      @pool.add_servers(servers.keys)
    end

    # The database type for this database object, the same as the adapter scheme
    # by default.  Should be overridden in adapters (especially shared adapters)
    # to be the correct type, so that even if two separate Database objects are
    # using different adapters you can tell that they are using the same database
    # type.  Even better, you can tell that two Database objects that are using
    # the same adapter are connecting to different database types.
    #
    #   Sequel.connect('jdbc:postgres://...').database_type
    #   # => :postgres
    def database_type
      adapter_scheme
    end
    
    # Disconnects all available connections from the connection pool.  Any
    # connections currently in use will not be disconnected. Options:
    # :server :: Should be a symbol specifying the server to disconnect from,
    #             or an array of symbols to specify multiple servers.
    #
    # Example:
    #
    #   DB.disconnect # All servers
    #   DB.disconnect(server: :server1) # Single server
    #   DB.disconnect(server: [:server1, :server2]) # Multiple servers
    def disconnect(opts = OPTS)
      pool.disconnect(opts)
    end

    # Should only be called by the connection pool code to disconnect a connection.
    # By default, calls the close method on the connection object, since most
    # adapters use that, but should be overwritten on other adapters.
    def disconnect_connection(conn)
      conn.close
    end

    # Dynamically remove existing servers from the connection pool. Only usable
    # when using a sharded connection pool
    #
    # servers should be symbols or arrays of symbols.  If a nonexistent server
    # is specified, it is ignored.  If no servers have been specified for
    # this database, no changes are made. If you attempt to remove the :default server,
    # an error will be raised.
    #
    #   DB.remove_servers(:f1, :f2)
    def remove_servers(*servers)
      unless sharded?
        raise Error, "cannot call Database#remove_servers on a Database instance that does not use a sharded connection pool"
      end

      h = @opts[:servers]
      servers.flatten.each{|s| Sequel.synchronize{h.delete(s)}}
      @pool.remove_servers(servers)
    end
    
    # An array of servers/shards for this Database object.
    #
    #   DB.servers # Unsharded: => [:default]
    #   DB.servers # Sharded:   => [:default, :server1, :server2]
    def servers
      pool.servers
    end

    # Connect to the given server/shard. Handles database-generic post-connection
    # setup not handled by #connect, using the :after_connect and :connect_sqls
    # options.
    def new_connection(server)
      conn = connect(server)
      opts = server_opts(server)

      if ac = opts[:after_connect]
        if ac.arity == 2
          ac.call(conn, server)
        else
          ac.call(conn)
        end
      end

      if cs = opts[:connect_sqls]
        cs.each do |sql|
          log_connection_execute(conn, sql)
        end
      end

      conn
    end

    # Returns true if the database is using a single-threaded connection pool.
    def single_threaded?
      @single_threaded
    end
    
    # The declared policy to use for SQL that cannot be confirmed to be
    # read-only.  This is used by #read_sql_server for statements executed
    # through raw SQL (e.g. Database#fetch or Dataset#with_sql) where the
    # statement cannot be proven to only read from the database.
    #
    # The default is :conservative, which sends such statements to the
    # :default (write) server.  :read_only is an explicit declaration that
    # unclassifiable statements in the application are read-only and can be
    # sent to the :read_only server.  A callable can also be used, it is
    # called with the SQL string and should return the server symbol to use
    # (or nil for the :default server).
    attr_reader :read_only_sql_policy

    # Set the policy to use for SQL that cannot be confirmed to be read-only.
    # See #read_only_sql_policy for the supported values.
    def read_only_sql_policy=(policy)
      @read_only_sql_policy = typecast_read_only_sql_policy(policy)
    end

    # Whether the given SQL statement can be confirmed to only read from the
    # database.  This is a conservative check that inspects the whole
    # statement (ignoring comments, quoted strings, and quoted identifiers),
    # instead of only looking at the first word.  Statements containing data
    # modification or schema modification keywords, locking clauses, references
    # to sequences, or multiple statements are not considered read-only.
    #
    #   DB.read_only_sql?('SELECT * FROM items') # => true
    #   DB.read_only_sql?('SELECT * FROM items FOR UPDATE') # => false
    #   DB.read_only_sql?('WITH d AS (DELETE FROM items RETURNING *) SELECT * FROM d') # => false
    def read_only_sql?(sql)
      read_only_sql_classification(read_only_sql_string(sql)) == :read_only
    end

    # The server to use when executing the given read query.  If the SQL can
    # be confirmed to be read-only, the :read_only server is used.  If the SQL
    # is confirmed to modify the database (or lock rows), the :default server
    # is used.  Otherwise, the declared #read_only_sql_policy is followed
    # (the :default server by default).
    def read_sql_server(sql)
      sql_string = read_only_sql_string(sql)

      case read_only_sql_classification(sql_string)
      when :read_only
        :read_only
      when :write
        :default
      else
        case policy = @read_only_sql_policy
        when :read_only
          :read_only
        when Proc, Method
          policy.call(sql_string || sql.to_s) || :default
        else
          :default
        end
      end
    end

    # For a transaction that is already active in the current thread, the
    # connection the transaction is using.  This is used to pin reads (and
    # savepoints) inside the transaction to the connection that is running the
    # transaction, instead of sending them to a replica.  Returns nil if no
    # transaction is active in the current thread.
    def current_transaction_connection
      if entry = current_transaction_connection_entry
        entry[0]
      end
    end
    private :current_transaction_connection

    # Whether a :read_only server that is separate from the :default server is
    # configured for this database.  This is only true if the :read_only
    # server was declared in the :servers option.  If it was not declared,
    # then there is no replica to route reads to, and the :read_only server
    # resolves to the :default (write) server.
    def distinct_read_only_server?
      @sharded && !!(@opts[:servers] && @opts[:servers].key?(:read_only))
    end
    private :distinct_read_only_server?

    # Acquires a database connection, yielding it to the passed block. This is
    # useful if you want to make sure the same connection is used for all
    # database queries in the block.  It is also useful if you want to gain
    # direct access to the underlying connection object if you need to do
    # something Sequel does not natively support.
    #
    # If a server option is given, acquires a connection for that specific
    # server, instead of the :default server.
    #
    # When a transaction is active in the current thread, a request for the
    # :read_only server is served using the connection running the
    # transaction.  This ensures that reads inside a transaction are not sent
    # to a replica (where they would not see the transaction's own writes),
    # and that a nested synchronize does not yield a connection belonging to
    # another server.  As that connection is owned by the transaction, it is
    # not returned to the connection pool when the block exits, and a failure
    # inside the block does not release the transaction's connection.
    # Requests for the :default server or other shards are unaffected.
    #
    #   DB.synchronize do |conn|
    #     # ...
    #   end
    def synchronize(server=nil, &block)
      server ||= :default

      if server == :read_only && distinct_read_only_server? && (conn = current_transaction_connection)
        return yield(conn)
      end

      @pool.hold(server, &block)
    end
    
    # Attempts to acquire a database connection.  Returns true if successful.
    # Will probably raise an Error if unsuccessful.  If a server argument
    # is given, attempts to acquire a database connection to the given
    # server/shard.
    def test_connection(server=nil)
      synchronize(server){|conn|}
      true
    end

    # Check whether the given connection is currently valid, by
    # running a query against it.  If the query fails, the
    # connection should probably be removed from the connection
    # pool.
    def valid_connection?(conn)
      sql = valid_connection_sql
      begin
        log_connection_execute(conn, sql)
      rescue Sequel::DatabaseError, *database_error_classes
        false
      else
        true
      end
    end

    private

    # If the given SQL object can be reduced to a single SQL string, return it.
    # Otherwise, return nil.  Used to ensure SQL classification only ever
    # inspects a real SQL string, never the result of calling #to_s on an
    # arbitrary object.  Statement arguments (bound separately) are never part
    # of the statement text, so they cannot change how keywords outside of them
    # are classified.
    def read_only_sql_string(sql)
      case sql
      when String
        sql
      when SQL::PlaceholderLiteralString
        str = sql.str
        if str.is_a?(Array)
          # Each placeholder is replaced by an opaque token, so statement
          # arguments can never change how keywords outside of them are
          # classified.
          str.join(' ? ')
        else
          str
        end
      end
    end

    # Classify the given SQL as :read_only (can be proven to only read from
    # the database), :write (provably modifies the database or locks rows), or
    # :unknown (cannot be safely classified).  This never looks at just the
    # first word: it strips comments, string literals, and quoted identifiers,
    # then requires the first token to be a read statement and the remaining
    # tokens to be free of data/schema modification, locking, or sequence
    # related keywords.  Multiple statements can never be classified as
    # :read_only.
    def read_only_sql_classification(sql)
      return :unknown unless sql.is_a?(String)

      tokens = read_only_sql_tokens(sql)
      return :unknown unless tokens
      return :unknown if tokens.empty?
      return :unknown if tokens.any?{|t| t == ';'}

      first = tokens.shift

      if READ_ONLY_SQL_LEADING_KEYWORDS.include?(first)
        # A statement starting with one of these can only be a read unless a
        # disallowed keyword is present later in the statement.
        if tokens.any?{|t| READ_ONLY_SQL_UNSAFE_KEYWORDS.include?(t)}
          :write
        else
          :read_only
        end
      elsif READ_ONLY_SQL_EXPLAIN_KEYWORDS.include?(first)
        if READ_ONLY_SQL_LEADING_KEYWORDS.include?(tokens.first) && !tokens.any?{|t| READ_ONLY_SQL_UNSAFE_KEYWORDS.include?(t)}
          :read_only
        else
          :unknown
        end
      elsif READ_WRITE_SQL_KEYWORDS.include?(first)
        :write
      else
        :unknown
      end
    end

    # Tokenize an SQL string for read-only classification.  Removes comments
    # and the contents of quoted strings and quoted identifiers.  Returns nil
    # if the string cannot be safely tokenized (e.g. unterminated quotes), in
    # which case the statement will not be considered read-only.
    def read_only_sql_tokens(sql)
      tokens = []
      s = sql
      len = s.length
      i = 0
      word = nil

      flush_word = proc do
        if word
          tokens << word.downcase
          word = nil
        end
      end

      while i < len
        c = s[i]

        case c
        when ' ', "\t", "\r", "\n", "\f", "\v"
          flush_word.call
          i += 1
        when '-'
          if s[i+1] == '-'
            flush_word.call
            i += 2
            i += 1 while i < len && s[i] != "\n"
          else
            flush_word.call
            tokens << '-'
            i += 1
          end
        when '/'
          if s[i+1] == '*'
            flush_word.call
            i += 2
            depth = 1
            while i < len && depth > 0
              if s[i] == '*' && s[i+1] == '/'
                depth -= 1
                i += 2
              elsif s[i] == '/' && s[i+1] == '*'
                depth += 1
                i += 2
              else
                i += 1
              end
            end
            return nil if depth > 0
          else
            flush_word.call
            tokens << '/'
            i += 1
          end
        when "'", '"', '`'
          quote = c
          flush_word.call
          i += 1
          loop do
            return nil if i >= len
            if s[i] == quote
              if s[i+1] == quote
                i += 2
              else
                i += 1
                break
              end
            else
              i += 1
            end
          end
          tokens << :quoted
        when '(' , ')', ',', ';'
          flush_word.call
          tokens << c
          i += 1
        when 'a'..'z', 'A'..'Z', '0'..'9', '_', '$', '#', '@', ':'
          word ||= String.new
          word << c
          i += 1
        else
          flush_word.call
          i += 1
        end
      end

      flush_word.call
      tokens
    end

    # Convert the given value to a supported read-only SQL policy.  Raises
    # an Error if the value is not supported.
    def typecast_read_only_sql_policy(policy)
      case policy
      when nil, :conservative, :default
        nil
      when :read_only, :readonly
        :read_only
      when Proc, Method
        policy
      else
        raise Error, "invalid read_only_sql_policy: #{policy.inspect} (should be :conservative, :read_only, or a callable)"
      end
    end

    # The default options for the connection pool.
    def connection_pool_default_options
      {}
    end

    # Return the options for the given server by merging the generic
    # options for all server with the specific options for the given
    # server specified in the :servers option.
    def server_opts(server)
      opts = if @opts[:servers] and server_options = @opts[:servers][server]
        case server_options
        when Hash
          @opts.merge(server_options)
        when Proc
          @opts.merge(server_options.call(self))
        else
          raise Error, 'Server opts should be a hash or proc'
        end
      elsif server.is_a?(Hash)
        @opts.merge(server)
      else
        @opts.dup
      end

      if pr = opts[:connect_opts_proc]
        pr.call(opts)
      end

      opts.delete(:servers)
      opts
    end

    # The SQL query to issue to check if a connection is valid.
    def valid_connection_sql
      @valid_connection_sql ||= select(nil).sql
    end
  end
end
