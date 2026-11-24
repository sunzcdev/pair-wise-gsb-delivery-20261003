# frozen_string_literal: true

require_relative "boot"

module OutboxDemo
  # Two *physically separate* SQLite files. No single database transaction can
  # ever span both, which is exactly the situation the outbox hand-off is built
  # for.
  BUSINESS_DB = File.join(TMP, "business.sqlite3")
  QUEUE_DB    = File.join(TMP, "queue.sqlite3")

  module Database
    module_function

    def connect!
      FileUtils.mkdir_p(TMP)

      # Business DB: plain ActiveRecord::Base connection (the "app" pool).
      ActiveRecord::Base.establish_connection(business_config)

      # Queue DB: Solid Queue's abstract record opens a second, independent
      # pool against the other SQLite file. The config is passed as a concrete
      # hash so no symbol resolution through Base.configurations is needed.
      SolidQueue.connects_to = { database: { writing: queue_config } }
      SolidQueue::Record.establish_connection(queue_config)
    end

    def business_config
      {
        adapter: "sqlite3",
        database: BUSINESS_DB,
        pool: 5,
        timeout: 5000
      }
    end

    def queue_config
      {
        adapter: "sqlite3",
        database: QUEUE_DB,
        pool: 5,
        timeout: 5000
      }
    end

    # Build the two schemas from scratch.
    def create_schema!
      create_business_schema
      create_queue_schema
    end

    def create_business_schema
      ActiveRecord::Base.connection.create_table :orders, force: :cascade do |t|
        t.string :sku, null: false
        t.integer :quantity, null: false
        t.string :state, null: false, default: "placed"
        t.timestamps
      end

      # The transactional outbox lives next to the business data, in the SAME
      # database, so inserting the order and its outbox row is one atomic
      # local transaction.
      ActiveRecord::Base.connection.create_table :outbox_messages, force: :cascade do |t|
        t.references :order, foreign_key: true
        t.string :job_class, null: false
        t.text :arguments, null: false
        t.string :queue_name, null: false, default: "default"
        t.string :status, null: false, default: "pending" # pending | dispatched
        t.string :solid_queue_job_id # Active Job id handed to Solid Queue
        t.bigint :solid_queue_provider_id # solid_queue_jobs.id after hand-off
        t.integer :attempts, null: false, default: 0
        t.text :last_error
        t.datetime :dispatched_at
        t.timestamps
        t.index :status
        t.index :solid_queue_job_id, unique: true
      end
    end

    def create_queue_schema
      # Load Solid Queue's own schema definition verbatim, but execute every
      # DDL call against the queue connection instead of ActiveRecord::Base.
      schema_path = File.join(ROOT, "lib", "generators", "solid_queue",
                              "install", "templates", "db", "queue_schema.rb")
      source = File.read(schema_path)
      # Strip the outer ActiveRecord::Schema[7.1].define(...) wrapper; its block
      # body only uses SchemaStatements methods, which we delegate to the queue
      # connection.
      body = source.sub(/\A.*?define\(version:.*?\)\s*do/m, "").sub(/end\s*\z/m, "")

      SolidQueue::Record.connection.execute("PRAGMA foreign_keys = ON")
      QueueSchemaDsl.new(SolidQueue::Record.connection).instance_eval(body, schema_path)

      # Idempotency guard for the recoverable hand-off. The stock Solid Queue
      # schema only puts a *non-unique* index on active_job_id; the relay always
      # re-enqueues with the outbox row's stable job id, so a unique index turns
      # a retried hand-off into a hard duplicate rejection instead of a second
      # job. One outbox row -> at most one Solid Queue job.
      SolidQueue::Record.connection.add_index(
        :solid_queue_jobs, :active_job_id,
        unique: true, name: "index_solid_queue_jobs_on_active_job_id_unique"
      )
    end

    # Minimal SchemaStatements surface used by queue_schema.rb, forwarded to a
    # specific connection so the official migration DDL lands in the queue DB.
    class QueueSchemaDsl
      def initialize(connection)
        @connection = connection
      end

      def method_missing(name, *args, **kwargs, &block)
        @connection.public_send(name, *args, **kwargs, &block)
      end

      def respond_to_missing?(*)
        true
      end
    end
  end
end
