# Logger

Logger is a simple but powerful logging utility to output messages in your Ruby program.

Logger has the following features:

 * Print messages to different levels such as `info` and `error`
 * Auto-rolling of log files
 * Setting the format of log messages
 * Specifying a program name in conjunction with the message

## Installation

Add this line to your application's Gemfile:

```ruby
gem 'logger'
```

And then execute:

    $ bundle

Or install it yourself as:

    $ gem install logger

## Usage

### Simple Example

```ruby
require 'logger'

# Create a Logger that prints to STDOUT
log = Logger.new(STDOUT)
log.debug("Created Logger")

log.info("Program finished")

# Create a Logger that prints to STDERR
error_log = Logger.new(STDERR)
error_log = error_log.error("fatal error")
```

### Compressing rotated log files

Rotated archives can optionally be gzip-compressed. The logger never starts a
compression thread itself; the caller schedules the work:

```ruby
require 'logger'

compressor = Logger::LogDevice::ArchiveCompressor.new
logger = Logger.new('app.log', 3, 10 * 1024 * 1024, archive_compressor: compressor)

# Rotation records the detached archive (never the active file):
logger.archived_logs # => ["app.log.0"]

# Compress on your own schedule, on the calling thread:
logger.compress_archived_logs
# "app.log.0" is replaced by a verified "app.log.0.gz"
```

The gzip archive is written to a temporary file and fully verified before it
is atomically renamed into place, so an interrupted compression never leaves a
truncated `.gz` and always keeps the original file. If the source path is
replaced during compression, the new object at that path is not deleted, and
submitting the same archive twice never compresses it concurrently.
Compression failures are reported separately from log writing failures.

## Development

After checking out the repo, run the following to install dependencies.

```
$ bin/setup
```

Then, run the tests as:

```
$ rake test
```

To install this gem onto your local machine, run

```
$ rake install
```

To release a new version, update the version number in `lib/logger/version.rb`, and then run

```
$ rake release
```

which will create a git tag for the version, push git commits and tags, and push the `.gem` file to [rubygems.org](https://rubygems.org).

## Advanced Development

### Run tests of a specific file

```
$ ruby test/logger/test_logger.rb
```

### Run tests filtering test methods by a name

`--name` option is available as:

```
$ ruby test/logger/test_logger.rb --name test_lshift
```

### Publish documents to GitHub Pages

```
$ rake gh-pages
```

Then, git commit and push the generated HTMLs onto `gh-pages` branch.

## Contributing

Bug reports and pull requests are welcome on GitHub at https://github.com/ruby/logger.

## License

The gem is available as open source under the terms of the [BSD-2-Clause](BSDL).
