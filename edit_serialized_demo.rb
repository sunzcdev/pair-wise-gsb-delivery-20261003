# Demonstrates Sequel's edit_serialized: block-style editing of serialized
# (JSON) attributes.  The block receives an isolated deep copy of the
# current value; the model value is only replaced when the block exits
# normally and the edited value passes validation for the column.
#
# Run with: ruby -Ilib edit_serialized_demo.rb
require_relative 'lib/sequel'
require 'json'

INITIAL_JSON = '{"theme":"dark","notifications":{"email":true,"channels":["alert","digest"]},"recent_searches":["ruby","sequel"]}'

DB = Sequel.mock(:fetch=>{:id=>1, :settings=>INITIAL_JSON}, :numrows=>1)
Sequel::Model.db = DB

class Item < Sequel::Model(DB[:items].columns(:id, :settings))
  set_primary_key :id
  plugin :serialization, :json, :settings
  plugin :serialization_modification_detection
  plugin :dirty

  def validate
    if settings && settings.has_key?('theme') && !settings['theme'].is_a?(String)
      errors.add(:settings, 'theme must be a String')
    end
  end
end

item = Item.first
puts "== initial value (nested structure) =="
pp item.settings

puts "\n== commit: edit nested hash/array in block, replaced atomically on success =="
leaked = nil
result = item.edit_serialized(:settings) do |s|
  leaked = s                                   # temp value held outside the block
  s['theme'] = 'light'                         # edit nested hash
  s['notifications']['channels'] << 'weekly'   # edit nested array
  s['recent_searches'].unshift('json')
  # the model value is untouched while the block runs:
  puts "inside block, model theme is still: #{item.settings['theme'].inspect}"
end
puts "edit_serialized => #{result}"
pp item.settings
puts "changed_columns: #{item.changed_columns.inspect}"
puts "column_changes (dirty): #{item.column_changes.inspect}"

puts "\n-- mutating the yielded object after the block cannot affect the model --"
leaked['theme'] = 'hacked'
leaked['notifications']['channels'].clear
puts "model theme: #{item.settings['theme'].inspect}"
puts "model channels: #{item.settings['notifications']['channels'].inspect}"

item.save_changes
puts "saved SQL: #{DB.sqls.grep(/UPDATE/).last}"
puts "changed_columns after save: #{item.changed_columns.inspect}"

puts "\n== rollback: edited value rejected by column validation, original kept =="
before = item.settings
result = item.edit_serialized(:settings) do |s|
  s['theme'] = 42                # violates the type constraint in validate
  s['recent_searches'].clear
end
puts "edit_serialized => #{result}"
puts "model untouched: #{item.settings == before}"
puts "theme is still: #{item.settings['theme'].inspect}"
puts "recent_searches is still: #{item.settings['recent_searches'].inspect}"
puts "model errors not polluted: #{item.errors.empty?}"
puts "changed_columns still empty: #{item.changed_columns.empty?}"

puts "\n== rollback: exception inside block, original kept =="
begin
  item.edit_serialized(:settings) do |s|
    s['theme'] = 'blue'
    s['notifications']['channels'] << 'sms'
    raise 'something went wrong'
  end
rescue RuntimeError => e
  puts "caught exception: #{e.message}"
end
puts "theme is still: #{item.settings['theme'].inspect}"
puts "channels are still: #{item.settings['notifications']['channels'].inspect}"

puts "\n== frozen input and shared subobjects =="
shared = {'volume'=>5}.freeze
item2 = Item.new
item2.settings = {'left'=>shared, 'right'=>shared}.freeze
result = item2.edit_serialized(:settings) do |s|
  s.frozen?                # => false, frozen structures are copied into mutable duplicates
  s['left']['volume'] = 7  # shared subobjects stay shared inside the copy
end
puts "edit_serialized => #{result}"
puts "committed value: #{item2.settings.inspect}"
puts "left/right still share one subobject: #{item2.settings['left'].equal?(item2.settings['right'])}"
puts "original frozen object unaffected: #{shared.inspect} (frozen: #{shared.frozen?})"

puts "\n== interplay with refresh: unsaved edits are discarded on refresh =="
item.edit_serialized(:settings){|s| s['theme'] = 'temporary'}
puts "theme before refresh: #{item.settings['theme'].inspect}"
item.refresh
puts "theme after refresh: #{item.settings['theme'].inspect}"
