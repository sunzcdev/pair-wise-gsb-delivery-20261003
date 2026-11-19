#!/usr/bin/env ruby
# encoding: utf-8

# The code generator targets Python 3 (Mako is required, e.g.
# `python -m pip install mako`). Override with the PYTHON environment
# variable when the interpreter has a different name.
python = ENV.fetch("PYTHON", "python3")

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
  # Python writes through text mode, which turns line endings into CRLF on
  # Windows. Normalize back to LF so the generated file is byte-stable and
  # matches the LF line endings stored in git.
  content = File.binread(path)
  File.binwrite(path, content.gsub("\r\n", "\n"))
  sh "ruby -c #{path}"
end
