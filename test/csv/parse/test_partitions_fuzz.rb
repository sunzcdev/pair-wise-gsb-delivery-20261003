# -*- coding: utf-8 -*-
# frozen_string_literal: false

require "tempfile"
require_relative "../helper"

class TestCSVPartitionsFuzz < Test::Unit::TestCase
  extend DifferentOFS

  private

  def build_options
    options = {}
    roll = rand
    if roll < 0.15
      options[:liberal_parsing] = true
    elsif roll < 0.3
      options[:liberal_parsing] = {backslash_quote: true}
    end
    sep = rand
    options[:row_sep] = "\r\n" if sep < 0.2
    options[:row_sep] = "|" if sep >= 0.2 && sep < 0.3
    options[:quote_char] = nil if rand < 0.08
    options[:col_sep] = "\u{00b7}" if rand < 0.05
    strip_roll = rand
    options[:strip] = true if strip_roll < 0.06
    options[:strip] = "\t" if strip_roll >= 0.06 && strip_roll < 0.1
    options
  end

  def merge(plan, options)
    merged = []
    plan.each { |partition| partition.each(**options) { |row| merged << row } }
    merged
  end

  public

  def test_random_partitions_roundtrip
    alphabet = ["a", "b", ",", '"', "\n", "\r\n",
                "\u{00e9}", "\u{4e2d}", "|", "\\", "  ", "\t"]
    target_sizes = [1, 2, 3, 5, 11, 64]
    200.times do
      data = Array.new(rand(0..120)) { alphabet.sample }.join
      next unless data.dup.force_encoding("UTF-8").valid_encoding?
      options = build_options
      options[:row_sep] ||= CSV.new(data.dup.force_encoding("UTF-8"),
                                    **options).row_sep
      file = Tempfile.new(["csv-pfuzz", ".csv"])
      file.binmode
      file.write(data)
      file.flush
      begin
        serial = begin
          CSV.parse(data.dup.force_encoding("UTF-8"),
                    encoding: "UTF-8", **options)
        rescue CSV::MalformedCSVError
          next
        end
        target_sizes.each do |size|
          plan = CSV.partitions(file.path, partition_size: size, **options)
          assert_equal(serial, merge(plan, options),
                       "data=#{data.inspect} options=#{options} " \
                       "size=#{size}")
        end
      ensure
        file.close
        file.unlink
      end
    end
  end
end
