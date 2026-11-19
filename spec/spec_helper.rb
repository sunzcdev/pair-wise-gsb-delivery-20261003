# encoding: binary

begin
  require 'bundler/setup'
  Bundler.require(:test)
rescue LoadError => e
  # No bundle configured: fall back to RSpec from the ambient gem environment.
  warn "spec_helper: no test bundle (#{e.message}); using ambient rspec"
  require 'rspec/its'
rescue StandardError => e
  raise unless defined?(Bundler::GemNotFound) && e.is_a?(Bundler::GemNotFound)

  warn "spec_helper: bundle is missing gems (#{e.message}); using ambient rspec"
  require 'rspec/its'
end

begin
  require 'simplecov'

  SimpleCov.start do
    add_filter '/spec/'
  end
rescue LoadError
end

$: << File.expand_path('../../lib', __FILE__)

require "amq/protocol"

puts "Running on #{RUBY_VERSION}"

RSpec.configure do |config|
  config.include AMQ::Protocol

  config.filter_run_when_matching :focus

  config.disable_monkey_patching!

  config.warnings = true
end
