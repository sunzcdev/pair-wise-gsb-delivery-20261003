# Safe write (concurrent recording)

When your CI splits a recording task across multiple processes, two of those
processes may start from the same cassette version and finish at roughly the
same time. A plain cassette overwrite would then silently drop the
interactions recorded by the process that finished first.

The `safe_write: true` cassette option routes the save through an explicit,
concurrency-aware entry point (`VCR::Cassette::Persisters::FileSystem.safe_write`):

1. Before writing, VCR re-reads the cassette file and compares it with the
   revision the cassette was loaded from (its "base" version).
2. Interactions recorded by another writer in the meantime are merged in.
3. If the two writers recorded different responses for the same request
   (matched with the cassette's `match_requests_on` matchers), VCR refuses to
   overwrite. The cassette that finished first is kept untouched and the
   losing interactions are written to a sibling `*.rejected-*` file.
4. A `VCR::Errors::CassetteWriteConflictError` is raised with full
   diagnostics: the conflicting request, the kept response, the rejected
   response, and the path of the rejected-interactions file.

The read/merge/write section is guarded by an exclusive file lock (so it works
across processes) and an in-process mutex (so it also works across threads).
The final commit is an atomic rename of a temporary file, so readers never see
a half-written cassette.

Safe saves are opt-in. Without the option, single-process recording behaves
exactly as it always did. Only the built-in `:file_system` persister supports
safe saves; using the option with another persister raises `NotImplementedError`.

## Background

_Given_ a file named "vcr_config.rb" with:

```ruby
require 'vcr'

VCR.configure do |c|
  c.hook_into :webmock
  c.cassette_library_dir = 'cassettes'
end
```

## Disjoint recordings are merged

_Given_ two recording processes both start from an empty cassette and one
records `GET http://example.com/a` while the other records
`GET http://example.com/b`, the finished cassette contains both interactions,
regardless of which process finishes first.

## Conflicting recordings keep the first response

_Given_ two recording processes both record `GET http://example.com/shared`
but receive different response bodies:

- The first process to finish keeps its response in `cassettes/example.yml`.
- The second process raises `VCR::Errors::CassetteWriteConflictError`.
- The rejected response is written to a file such as
  `cassettes/example.rejected-202611231200000000000.yml` for inspection.

Inspect the rejected interactions, then either delete the rejected file or
manually merge the responses you want to keep.

## Enabling it

```ruby
VCR.use_cassette('example', record: :new_episodes, safe_write: true) do
  # HTTP interactions recorded here participate in the safe merge on eject.
end
```

You can also make it the default for every cassette:

```ruby
VCR.configure do |c|
  c.default_cassette_options = { safe_write: true }
end
```
