require "bundler"
Bundler.setup

require "rake"
require "bundler/gem_tasks"
require "rspec/core/rake_task"

$LOAD_PATH.unshift File.expand_path("../lib", __FILE__)
require "amq/protocol/version"

def extension
  RUBY_PLATFORM =~ /darwin/ ? "bundle" : "so"
end

def compile!
  puts "Compiling native extensions..."
  Dir.chdir(Pathname(__FILE__).dirname + "ext/") do
    `bundle exec ruby extconf.rb`
    `make`
    `cp client.#{extension} ../lib/amq/protocol/native/`
  end
end

RSpec::Core::RakeTask.new("spec") do |spec|
  spec.pattern = "spec/**/*_spec.rb"
end

RSpec::Core::RakeTask.new("clean_spec") do |spec|
  spec.pattern = "spec/**/*_spec.rb"
end

task :compile do
  compile!
end

desc "Verify that every bundled protocol description generates stable output"
task :verify_descriptions do
  require "amq/protocol"

  files = Dir.glob(File.expand_path("lib/amq/protocol/spec/data/*.json", __dir__))
  raise "no bundled protocol descriptions found" if files.empty?

  files.each do |path|
    first = AMQ::Protocol::Generator.manifest(AMQ::Protocol::Spec::Loader.load_file(path))
    second = AMQ::Protocol::Generator.manifest(AMQ::Protocol::Spec::Loader.load_file(path))
    raise "#{path} does not generate a stable manifest" unless first == second

    puts "#{File.basename(path)}: #{first.lines.first.strip}"
  end
end

task :clean do
  puts "Cleaning out native extensions..."
  begin
    Dir.chdir(Pathname(__FILE__).dirname + "lib/amq-protocol/native") do
      `rm client.#{extension}`
    end
  rescue Exception => e
    puts e.message
  end
end

task :default => [:compile, :spec, :clean, :clean_spec]
