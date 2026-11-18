# Pstore

PStore implements a file based persistence mechanism based on a Hash.  User
code can store hierarchies of Ruby objects (values) into the data store file
by name (keys).  An object hierarchy may be just a single object.  User code
may later read values back from the data store or even update data, as needed.

The transactional behavior ensures that any changes succeed or fail together.
This can be used to ensure that the data store is not left in a transitory
state, where some values were updated but others were not.

Behind the scenes, Ruby objects are stored to the data store file with
Marshal.  That carries the usual limitations.  Proc objects cannot be
marshalled, for example.

## Installation

Add this line to your application's Gemfile:

```ruby
gem 'pstore'
```

And then execute:

    $ bundle

Or install it yourself as:

    $ gem install pstore

## Usage

```ruby
require "pstore"

# a mock wiki object...
class WikiPage
  def initialize( page_name, author, contents )
    @page_name = page_name
    @revisions = Array.new

    add_revision(author, contents)
  end

  attr_reader :page_name

  def add_revision( author, contents )
    @revisions << { :created  => Time.now,
                    :author   => author,
                    :contents => contents }
  end

   def wiki_page_references
    [@page_name] + @revisions.last[:contents].scan(/\b(?:[A-Z]+[a-z]+){2,}/)
  end

   # ...
end

# create a new page...
home_page = WikiPage.new( "HomePage", "James Edward Gray II",
                          "A page about the JoysOfDocumentation..." )

 # then we want to update page data and the index together, or not at all...
wiki = PStore.new("wiki_pages.pstore")
wiki.transaction do  # begin transaction; do all of this or none of it
  # store page...
  wiki[home_page.page_name] = home_page
  # ensure that an index has been created...
  wiki[:wiki_index] ||= Array.new
  # update wiki index...
  wiki[:wiki_index].push(*home_page.wiki_page_references)
end                   # commit changes to wiki data store file

 ### Some time later... ###

 # read wiki data...
wiki.transaction(true) do  # begin read-only transaction, no changes allowed
  wiki.roots.each do |data_root_name|
    p data_root_name
    p wiki[data_root_name]
  end
end
```

### Nested transactions

Nested transactions are opt-in. By default, calling `transaction` inside an
active transaction raises `PStore::Error`. Pass `nested_transactions: true`
to `PStore.new` to enable savepoint-scoped nesting:

```ruby
store = PStore.new("nested.pstore", nested_transactions: true)
store.transaction do              # outermost level owns the file write
  store[:a] = 1
  store.transaction do            # inner level gets its own savepoint
    store[:b] = 2
    store.abort                   # discards only :b, outer level keeps going
  end
  store[:c] = 3
end
# Persisted data: {a: 1, c: 3}
```

- An inner level that finishes normally (or calls `commit`) merges its
  changes into the surrounding level and only exits its own block.
- An inner `abort`, or an exception raised inside an inner level, restores
  that level's savepoint, discarding only its own changes; exceptions then
  continue to unwind, rolling back each level they cross.
- `commit`/`abort` are scoped to the level that calls them.
- A read-only outer transaction cannot be upgraded to a read-write one;
  an inner writable transaction raises `PStore::Error`.
- Only the outermost level opens, locks, and writes the file, and only when
  it completes without being aborted or raising.

## Development

After checking out the repo, run `bin/setup` to install dependencies. Then, run `rake test` to run the tests. You can also run `bin/console` for an interactive prompt that will allow you to experiment.

To install this gem onto your local machine, run `bundle exec rake install`. To release a new version, update the version number in `version.rb`, and then run `bundle exec rake release`, which will create a git tag for the version, push git commits and tags, and push the `.gem` file to [rubygems.org](https://rubygems.org).

## Contributing

Bug reports and pull requests are welcome on GitHub at https://github.com/ruby/pstore.
