ActiveRecord::Schema[7.2].define(version: 1) do
  create_table "solid_cache_counters", force: :cascade do |t|
    t.string "name", null: false
    t.integer "value", limit: 8, null: false, default: 0
    t.index ["name"], name: "index_solid_cache_counters_on_name", unique: true
  end

  create_table "solid_cache_entries", force: :cascade do |t|
    t.binary "key", limit: 1024, null: false
    t.binary "value", limit: 536870912, null: false
    t.datetime "created_at", null: false
    t.integer "key_hash", limit: 8, null: false
    t.integer "byte_size", limit: 4, null: false
    t.index ["byte_size"], name: "index_solid_cache_entries_on_byte_size"
    t.index ["key_hash", "byte_size"], name: "index_solid_cache_entries_on_key_hash_and_byte_size"
    t.index ["key_hash"], name: "index_solid_cache_entries_on_key_hash", unique: true
  end
end
