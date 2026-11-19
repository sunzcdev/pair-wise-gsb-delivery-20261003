#!/usr/bin/env ruby
require 'erb'
require_relative '../lib/roda/version'

descriptions = {
  index: "Roda is a lightweight and productive framework for building web applications in Ruby.",
  documentation: "Documentation and Tutorials for Roda, the Routing Tree Web Toolkit for Ruby",
  development: "Contributing to Roda, the Routing Tree Web Toolkit for Ruby",
  compare_to_sinatra: "A brief breakdown of how Roda stacks up against Sinatra for web development.",
}

Dir.chdir(File.dirname(__FILE__))
erb = ERB.new(File.read('layout.erb'))
Dir['pages/*.erb'].each do |page|
  public_loc = "#{page.gsub(/\Apages\//, 'public/').sub('.erb', '.html')}"
  content = content = ERB.new(File.read(page)).result(binding)
  current_page = File.basename(page.sub('.erb', ''))
  description = description = descriptions[current_page.to_sym]
  File.open(public_loc, 'wb'){|f| f.write(erb.result(binding))}
end


