# frozen_string_literal: true
# Verify belongs_to counter_cache alignment when a record's parent is moved
# A -> B -> C inside one transaction, incl. savepoint rollback and nil parent.
$LOAD_PATH.unshift File.expand_path("activesupport/lib", __dir__)
$LOAD_PATH.unshift File.expand_path("activemodel/lib", __dir__)
$LOAD_PATH.unshift File.expand_path("activerecord/lib", __dir__)

require "active_record"
require "active_support/notifications"

puts "ActiveRecord version: #{ActiveRecord::VERSION::STRING}"

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

class Parent < ActiveRecord::Base
  has_many :children
end

class Child < ActiveRecord::Base
  belongs_to :parent, counter_cache: true, optional: true
end

# Capture every SQL statement that touches the counter column
$counter_sql = []
ActiveSupport::Notifications.subscribe("sql.active_record") do |_n, _s, _f, _id, payload|
  sql = payload[:sql]
  $counter_sql << sql if sql.include?("children_count") && payload[:name] != "SCHEMA"
end

$failures = 0
def report(label)
  puts "--- #{label} ---"
  Parent.order(:id).each do |p|
    actual  = Child.where(parent_id: p.id).count          # real rows in SQLite
    counter = Parent.where(id: p.id).pick(:children_count) # counter column, fresh from DB
    ok = (counter == actual)
    $failures += 1 unless ok
    puts format("  [%s] parent=%s counter=%d actual=%d", ok ? "OK" : "MISMATCH", p.name, counter, actual)
  end
end

def dump_counter_sql(label)
  puts "  counter SQL emitted during #{label} (#{$counter_sql.size} statements):"
  $counter_sql.each { |s| puts "    #{s}" }
  $counter_sql.clear
end

a = Parent.create!(name: "A")
b = Parent.create!(name: "B")
c = Parent.create!(name: "C")
$counter_sql.clear

puts "== Scenario 1: child created under A, then moved A -> B -> C in ONE transaction =="
child = Child.create!(name: "kid", parent: a)
dump_counter_sql("create")
report("after create under A")

Parent.transaction do
  child.update!(parent: b)   # move 1: A -> B
  dump_counter_sql("move A->B (save 1)")
  child.update!(parent: c)   # move 2: B -> C
  dump_counter_sql("move B->C (save 2)")
end
report("after A->B->C committed")

puts
puts "== Scenario 2: savepoint rollback =="
puts "before: db parent_id=#{Child.find(child.id).parent_id} (c=#{c.id})"
Parent.transaction do
  child.update!(parent: a)                       # C -> A
  dump_counter_sql("move C->A")
  Parent.transaction(requires_new: true) do      # savepoint
    child.update!(parent: b)                     # A -> B, rolled back below
    dump_counter_sql("move A->B inside savepoint")
    raise ActiveRecord::Rollback
  end
  puts "  after savepoint rollback, object state:"
  puts "    in-memory child.parent_id            = #{child.parent_id.inspect} (a=#{a.id}, b=#{b.id})"
  puts "    child.parent_id_in_database          = #{child.parent_id_in_database.inspect}"
  puts "    child.saved_change_to_parent_id?     = #{child.saved_change_to_parent_id?.inspect}"
  puts "    child.attribute_before_last_save(:parent_id) = #{child.attribute_before_last_save(:parent_id).inspect}"
  puts "    DB parent_id (fresh query)           = #{Child.where(id: child.id).pick(:parent_id).inspect}"
  puts "    association(:parent).saved_change_to_target? = #{child.association(:parent).saved_change_to_target?.inspect}"
end
report("after outer transaction commit")

puts
puts "== Scenario 3: nil parent end =="
child.reload
puts "reloaded child.parent_id = #{child.parent_id.inspect}"
child.update!(parent: nil)                       # A -> nil: decrement only
dump_counter_sql("move A->nil")
report("after move to nil")
child.update!(parent: c)                         # nil -> C: increment only
dump_counter_sql("move nil->C")
report("after move nil->C")

puts
puts "== Scenario 4: stale in-memory old parent (decrement goes through a scope, not the object) =="
stale_a = Parent.find(a.id)                      # separate in-memory copy
orphan  = Child.create!(name: "tmp", parent: a)  # a now has 1 child in DB
$counter_sql.clear
child2 = orphan
child2.update!(parent: b)                        # decrement A via scoped UPDATE
dump_counter_sql("move A->B with stale A object in memory")
puts "  stale_a.children_count (in memory, not reloaded) = #{stale_a.children_count}"
puts "  a.children_count in DB = #{Parent.where(id: a.id).pick(:children_count)}"
report("after moving with stale old-parent object")
child2.destroy!
$counter_sql.clear
report("after cleanup destroy")

puts
puts "== Scenario 5: savepoint rollback, then save again in same transaction (replayed dirty state) =="
child.reload
Parent.transaction do
  Parent.transaction(requires_new: true) do
    child.update!(parent: b)                     # C -> B, will roll back
    raise ActiveRecord::Rollback
  end
  puts "  after rollback: in-memory parent_id=#{child.parent_id.inspect}, DB parent_id=#{Child.where(id: child.id).pick(:parent_id).inspect}"
  child.update!(name: "kid-renamed")             # dirty parent_id is replayed here
  dump_counter_sql("post-rollback save")
  puts "  after re-save: DB parent_id=#{Child.where(id: child.id).pick(:parent_id).inspect}"
end
report("after rollback + re-save commit")

puts
puts($failures.zero? ? "ALL CHECKS PASSED" : "#{$failures} MISMATCH(ES) FOUND")
exit($failures.zero? ? 0 : 1)
