# frozen_string_literal: true

require "cases/helper"
require "active_record/errors"

class ErrorsTest < ActiveRecord::TestCase
  def test_can_be_instantiated_with_no_args
    base = ActiveRecord::ActiveRecordError
    error_klasses = ObjectSpace.each_object(Class).select { |klass| klass < base }

    expected_to_be_initializable_with_no_args = error_klasses - [
      ActiveRecord::AmbiguousSourceReflectionForThroughAssociation,
      ActiveRecord::DeprecatedAssociationError
    ]
    assert_nothing_raised do
      expected_to_be_initializable_with_no_args.each do |error_klass|
        error_klass.new.inspect
      rescue ArgumentError
        raise "Instance of #{error_klass} can't be initialized with no arguments"
      end
    end
  end

  def test_stale_object_error_defaults_attempted_attributes_to_an_empty_hash
    error = ActiveRecord::StaleObjectError.new
    assert_equal({}, error.attempted_attributes)

    record = Object.new
    with_attributes = ActiveRecord::StaleObjectError.new(
      record, "update", attempted_attributes: { "name" => "value" }
    )
    assert_same record, with_attributes.record
    assert_equal "update", with_attributes.attempted_action
    assert_equal({ "name" => "value" }, with_attributes.attempted_attributes)
  end
end
