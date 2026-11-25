---
chapter: SQL
title: Schemas
---

The SQL adapter adds its own schema types and association declarations to the
built-in [Relation Schema](/learn/core/5.2/schemas) feature.

## Inferring Attributes

If you don't want to declare all attributes explicitly, you can tell rom-sql to
infer attributes from an existing schema.

Inference will define normal attributes, foreign keys and primary key (even when
it's a composite primary key).

To infer a schema automatically:

``` ruby
require 'rom-sql'

class Users < ROM::Relation[:sql]
  schema(infer: true) # that's it
end
```

## Coercions

Relations and commands can coerce output and input data automatically based on your schema attributes.
Default attribute types in schemas are used for input coercions in commands, if you want to apply additional
coercions when relations read their data, you can do it via `:read` type in schema definitions:

``` ruby
class Posts < ROM::Relation[:sql]
  schema(infer: true) do
    attribute :status, Types::String, read: Types.Constructor(Symbol, &:to_sym)
  end
end

id = posts.insert(title: 'Hello World', status: :draft)

posts.by_pk(1).one
# => {:id => 1, :title => "Hello World", status: :draft }
```

## Defaults when inserting records

Create commands do not apply schema-level defaults to attributes you omit.
An omitted attribute is simply left out of the `INSERT` statement, so the
database can apply its own column default - including defaults that are
expressions evaluated server-side:

``` ruby
class Users < ROM::Relation[:sql]
  schema(infer: true) do
    attribute :created_at, Types::Time
  end
end

# `created_at` is omitted, so the column default applies:
users.command(:create).call(name: 'Jane')
# => {id: 1, name: "Jane", created_at: 2025-01-08 10:00:00 UTC}
```

Passing `nil` explicitly is different: the value is kept as `nil` and the
regular column constraints apply. Inserting `nil` into a `NOT NULL` column
raises an error:

``` ruby
users.command(:create).call(name: 'Jane', created_at: nil)
# => raises ROM::SQL::NotNullConstraintError
```

Returned tuples are read back from the database, so they reflect what was
actually stored - including values generated server-side. When the inserted
row cannot be read back (for example, when the table has no primary key),
`ROM::SQL::Commands::Unreadable` is returned in its place instead of guessing
values in Ruby.

## PostgreSQL Types

When you define relation schema attributes using custom PG types, the values
will be automatically coerced before executing commands, so you don't have to
handle that yourself.

``` ruby
require 'rom-sql'

class Users < ROM::Relation[:sql]
  schema do
    attribute :meta, Types::PG::JSONB
    attribute :tags, Types::PG::Array('varchar')
    attribute :info, Types::PG::HStore
  end
end

Users.schema[:meta][{ name: 'Jane' }].class
# Sequel::Postgres::JSONBHash

Users.schema[:meta][[1, 2, 3]].class
# Sequel::Postgres::JSONBArray

Users.schema[:tags][%w(red green blue)].class
# => Sequel::Postgres::PGArray

Users.schema[:info][{ some: 'info' }].class
# => Sequel::Postgres::HStore
```

For getting `hstore` to work be sure you have the `pg_hstore` extension loaded.
