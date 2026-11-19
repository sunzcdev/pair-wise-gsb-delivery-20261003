#!/usr/bin/env ruby
# encoding: utf-8

# Generates Ruby sources from protocol description documents.
#
#   ./generate.rb                      regenerate the legacy 0.9.1 namespace
#   ./generate.rb --versioned          also emit version namespaces
#   ./generate.rb --describe PATH      print a description manifest
#
# Description documents are read once and never mutated; generating the same
# description twice produces byte-identical output.

$LOAD_PATH.unshift File.expand_path("lib", __dir__)

require "optparse"

mode = :legacy
describe_path = nil

OptionParser.new do |opts|
  opts.banner = "Usage: ./generate.rb [--versioned] [--describe PATH]"
  opts.on("--versioned", "Emit per-version namespaces") { mode = :versioned }
  opts.on("--describe PATH", "Print the manifest of a description document") { |path| describe_path = path }
end.parse!

require "amq/protocol"

if describe_path
  description = AMQ::Protocol::Spec::Loader.load_file(describe_path)
  print AMQ::Protocol::Generator.manifest(description)
  exit 0
end

if mode == :versioned
  description = AMQ::Protocol.descriptions.amqp_0_9_1
  $stdout.print AMQ::Protocol::Generator.generate(description, "AMQ::Protocol::Versions::AMQP091")
  exit 0
end

# rabbitmq-codegen is Python 3 compatible and so is
# the code in this repo but Mako still fails with 3.6 as of May 2017 :( MK.
python = ENV.fetch("PYTHON", "python2")

def sh(*args)
  system(*args)
end

extensions = []

spec = "codegen/rabbitmq-codegen/amqp-rabbitmq-0.9.1.json"
unless File.exist?(spec)
  sh "git submodule update --init"
end

path = "lib/amq/protocol/client.rb"
puts "Running '#{python} ./codegen/codegen.py client #{spec} #{extensions.join(' ')} #{path}'"
sh "#{python} ./codegen/codegen.py client #{spec} #{extensions.join(' ')} #{path}"
if File.file?(path)
  sh "ruby -c #{path}"
end
