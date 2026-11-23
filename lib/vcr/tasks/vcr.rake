namespace :vcr do
  desc "Migrate cassettes from the VCR 1.x format to the VCR 2.x format."
  task :migrate_cassettes do
    dir = ENV.fetch('DIR') { raise "You must pass the cassette library directory as DIR=<directory>" }
    require 'vcr/cassette/migrator'
    VCR::Cassette::Migrator.new(dir).migrate!
  end

  desc "Review, then publish, a directory-to-directory cassette migration (DIR=<source> TARGET=<target>)."
  task :migrate_directory do
    dir    = ENV.fetch('DIR')    { raise "You must pass the source cassette directory as DIR=<directory>" }
    target = ENV.fetch('TARGET') { raise "You must pass the target cassette directory as TARGET=<directory>" }
    require 'vcr/cassette/migrator'
    VCR::Cassette::Migrator.migrate_directory(dir, target)
  end
end

