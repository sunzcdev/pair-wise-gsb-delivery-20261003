# frozen_string_literal: true
$LOAD_PATH.unshift File.expand_path("activesupport/lib", __dir__)
$LOAD_PATH.unshift File.expand_path("activemodel/lib", __dir__)
$LOAD_PATH.unshift File.expand_path("activerecord/lib", __dir__)
require "active_record"
require "active_support/notifications"

ActiveRecord::Base.establish_connection(adapter: "sqlite3", database: ":memory:")
ActiveRecord::Schema.verbose = false
ActiveRecord::Schema.define do
  create_table :parents do |t|
    t.string :name
    t.integer :children_count, null: false, default: 0
  end
  create_table :children do |t|
    t.string :name
    t.references :parent
  end
end
class Parent < ActiveRecord::Base; has_many :children; end
class Child < ActiveRecord::Base; belongs_to :parent, counter_cache: true, optional: true; end

$sql = []
ActiveSupport::Notifications.subscribe("sql.active_record") do |_n, _s, _f, _id, p|
  next if p[:name] == "SCHEMA" || p[:sql] =~ /TRANSACTION|SAVEPOINT|RELEASE/i
  $sql << "#{p[:sql]}  binds=#{(p[:type_casted_binds] || []).inspect}"
end

def alignment(label)
  puts label
  Parent.order(:id).each do |p|
    actual  = Child.where(parent_id: p.id).count
    counter = Parent.where(id: p.id).pick(:children_count)
    puts format("  [%s] %s counter=%d actual=%d", counter == actual ? "OK" : "MISMATCH", p.name, counter, actual)
  end
end

a = Parent.create!(name: "A"); b = Parent.create!(name: "B"); c = Parent.create!(name: "C")

puts "== Case 1: savepoint rollback, then WITHOUT reload move back to C in same transaction =="
child = Child.create!(name: "k1", parent: c)
$sql.clear
Parent.transaction do
  Parent.transaction(requires_new: true) do
    child.update!(parent: b)   # C -> B, rolled back
    raise ActiveRecord::Rollback
  end
  # in-memory parent_id=2(B), DB=3(C), tracker thinks DB=2
  child.update!(parent: c)     # looks like B -> C to the dirty tracker
end
puts "counter SQL after rollback (should be none until the second save):"
$sql.each { |s| puts "  #{s}" }
alignment("alignment (expect corruption: C double-counted, B decremented below zero):")

puts
puts "== Case 2: same, but reload right after the savepoint rollback =="
child2 = Child.create!(name: "k2", parent: c)
alignment("baseline:")
Parent.transaction do
  Parent.transaction(requires_new: true) do
    child2.update!(parent: b)
    raise ActiveRecord::Rollback
  end
  child2.reload                # resync object with DB
  child2.update!(parent: c)    # no-op now (already C), or correct move
  child2.update!(parent: a)    # a real move C -> A
end
alignment("alignment after reload + real move C->A:")

puts
puts "== Case 3: outer transaction rollback (not savepoint) =="
child3 = Child.create!(name: "k3", parent: c)
begin
  Parent.transaction do
    child3.update!(parent: a)
    child3.update!(parent: b)
    raise "boom"
  end
rescue => e
  puts "  rolled back with: #{e.message}"
end
puts "  in-memory parent_id=#{child3.parent_id.inspect} (b=#{b.id}, c=#{c.id}), DB=#{Child.where(id: child3.id).pick(:parent_id).inspect}"
alignment("alignment after outer rollback:")
