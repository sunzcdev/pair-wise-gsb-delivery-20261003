require 'test_helper'

class SensitivePathsTest < Minitest::Test
  def schema
    JSONSchemer.schema({
      'type' => 'object',
      'properties' => {
        'a/b~c' => {
          'type' => 'object',
          'properties' => {
            'token' => { 'minLength' => 10 }
          }
        },
        'creds' => {
          'type' => 'array',
          'items' => {
            'type' => 'object',
            'properties' => {
              'password' => { 'minLength' => 10 },
              'label' => { 'minLength' => 5 }
            }
          }
        },
        'name' => { 'minLength' => 5 }
      }
    })
  end

  def instance
    {
      'a/b~c' => { 'token' => 'tk' },
      'creds' => [{ 'password' => 'short', 'label' => 'ab' }],
      'name' => 'ab'
    }
  end

  def sensitive_paths
    ['/a~1b~0c', '/creds', '/creds/0/password']
  end

  def assert_redacted(value)
    refute_includes(value.inspect, 'short')
    refute_includes(value.inspect, 'tk')
  end

  def test_classic_redacts_sensitive_data_and_keeps_locations_keyword_type
    errors = schema.validate(instance, :sensitive_paths => sensitive_paths).to_a

    assert_equal(4, errors.size)

    by_pointer = errors.to_h { |error| [error.fetch('data_pointer'), error] }

    token_error = by_pointer.fetch('/a~1b~0c/token')
    assert_equal(JSONSchemer::Redactor::REDACTED, token_error.fetch('data'))
    assert_equal('minLength', token_error.fetch('type'))
    assert_equal('string length at `/a~1b~0c/token` is less than: 10', token_error.fetch('error'))

    password_error = by_pointer.fetch('/creds/0/password')
    assert_equal(JSONSchemer::Redactor::REDACTED, password_error.fetch('data'))

    label_error = by_pointer.fetch('/creds/0/label')
    assert_equal(JSONSchemer::Redactor::REDACTED, label_error.fetch('data'))

    name_error = by_pointer.fetch('/name')
    assert_equal('ab', name_error.fetch('data'))

    errors.each { |error| assert_redacted(error) }
  end

  def test_basic_detailed_and_verbose_redact_nested_errors
    %w[basic detailed verbose].each do |output_format|
      output = schema.validate(
        instance,
        :output_format => output_format,
        :resolve_enumerators => true,
        :sensitive_paths => sensitive_paths
      )

      assert_equal(false, output.fetch('valid'))
      assert_redacted(output)

      locations = collect_instance_locations(output)
      assert_includes(locations, '/a~1b~0c/token')
      assert_includes(locations, '/creds/0/password')
      assert_includes(locations, '/creds/0/label')
      assert_includes(locations, '/name')
    end
  end

  def test_parent_error_data_is_redacted
    schemer = JSONSchemer.schema({
      'anyOf' => [
        {
          'type' => 'object',
          'properties' => { 'name' => { 'minLength' => 10 } },
          'required' => ['name']
        },
        {
          'type' => 'object',
          'properties' => { 'password' => { 'minLength' => 10 } }
        }
      ]
    })
    data = { 'password' => 'short' }

    error = schemer.validate(data, :sensitive_paths => ['/password']).to_a.first

    assert_equal('', error.fetch('data_pointer'))
    assert_equal({ 'password' => JSONSchemer::Redactor::REDACTED }, error.fetch('data'))

    %w[basic detailed verbose].each do |output_format|
      output = schemer.validate(
        data,
        :output_format => output_format,
        :resolve_enumerators => true,
        :sensitive_paths => ['/password']
      )
      assert_redacted(output)
    end
  end

  def test_root_pointer_redacts_whole_instance
    schemer = JSONSchemer.schema({ 'type' => 'string', 'minLength' => 20 })
    error = schemer.validate('badsecret', :sensitive_paths => ['']).to_a.first
    assert_equal(JSONSchemer::Redactor::REDACTED, error.fetch('data'))
  end

  def test_instance_derived_error_description_is_replaced_without_value
    require 'yaml'
    require 'i18n'

    JSONSchemer.remove_class_variable(:@@i18n) if JSONSchemer.class_variable_defined?(:@@i18n)

    translations = {
      'en' => {
        'json_schemer' => {
          'errors' => {
            '#/minLength' => 'the value %{instance} at %{formattedInstanceLocation} is too short'
          }
        }
      }
    }

    with_translations(translations) do
      schemer = JSONSchemer.schema({ 'minLength' => 20 })

      classic_error = schemer.validate('hunter2secret', :sensitive_paths => ['']).to_a.first
      assert_equal('string length at root is less than: 20', classic_error.fetch('error'))
      refute_includes(classic_error.fetch('error'), 'hunter2secret')

      basic_output = schemer.validate('hunter2secret', :output_format => 'basic', :sensitive_paths => [''])
      assert_equal('value at root does not match schema', basic_output.fetch('error'))
      refute_includes(basic_output.fetch('error'), 'hunter2secret')
    end
  ensure
    JSONSchemer.remove_class_variable(:@@i18n) if JSONSchemer.class_variable_defined?(:@@i18n)
  end

  def test_x_error_copy_is_not_modified
    schemer = JSONSchemer.schema({
      'type' => 'string',
      'x-error' => 'caller copy mentioning %{instance}'
    })

    error = schemer.validate(42, :sensitive_paths => ['']).to_a.first
    assert_equal('caller copy mentioning 42', error.fetch('error'))
    assert_equal(true, error.fetch('x-error'))

    basic_output = schemer.validate(42, :output_format => 'basic', :sensitive_paths => [''])
    assert_equal('caller copy mentioning 42', basic_output.fetch('error'))
  end

  def test_non_leaking_descriptions_are_unchanged
    schemer = JSONSchemer.schema({ 'minLength' => 10 })
    error = schemer.validate('short', :sensitive_paths => ['/other']).to_a.first
    assert_equal('string length at root is less than: 10', error.fetch('error'))
  end

  def test_details_are_redacted
    redactor = JSONSchemer::Redactor.new(
      { 'password' => 'badsecret' },
      ['/password']
    )

    details = {
      'missing_keys' => ['name'],
      'echo' => 'badsecret',
      'nested' => { 'again' => 'badsecret', 'keep' => 'visible' },
      'list' => ['badsecret', 'visible']
    }

    assert_equal(
      {
        'missing_keys' => ['name'],
        'echo' => JSONSchemer::Redactor::REDACTED,
        'nested' => { 'again' => JSONSchemer::Redactor::REDACTED, 'keep' => 'visible' },
        'list' => [JSONSchemer::Redactor::REDACTED, 'visible']
      },
      redactor.redact(details)
    )
    refute_includes(redactor.redact(details).inspect, 'badsecret')
  end

  def test_redactor_hides_whole_object_and_array_subtrees
    data = {
      'credentials' => { 'password' => 'hunter2secret', 'token' => 'tk' },
      'tokens' => ['tk-one', 'tk-two'],
      'name' => 'visible'
    }
    redactor = JSONSchemer::Redactor.new(data, ['/credentials', '/tokens'])

    assert_equal(
      {
        'credentials' => JSONSchemer::Redactor::REDACTED,
        'tokens' => JSONSchemer::Redactor::REDACTED,
        'name' => 'visible'
      },
      redactor.instance
    )
    assert_equal(JSONSchemer::Redactor::REDACTED, redactor.instance_at('/credentials/password'))
    assert_equal(JSONSchemer::Redactor::REDACTED, redactor.instance_at('/tokens/1'))
    assert_equal('visible', redactor.instance_at('/name'))
    assert_nil(redactor.instance_at('/missing'))
    assert_nil(redactor.instance_at('/name/x'))
  end

  def test_redactor_escaped_pointer_keys
    data = { 'a/b~c' => { '~x' => 'secret-value' } }
    redactor = JSONSchemer::Redactor.new(data, ['/a~1b~0c/~x'])

    assert_equal(
      { 'a/b~c' => { '~x' => JSONSchemer::Redactor::REDACTED } },
      redactor.instance
    )
  end

  def test_redactor_array_indices_and_out_of_bounds
    data = { 'items' => ['keep-zero', 'bad-one', { 'keep' => 'two' }] }
    redactor = JSONSchemer::Redactor.new(data, ['/items/1', '/items/9'])

    assert_equal(
      { 'items' => ['keep-zero', JSONSchemer::Redactor::REDACTED, { 'keep' => 'two' }] },
      redactor.instance
    )
    assert_equal('keep-zero', redactor.instance_at('/items/0'))
    assert_nil(redactor.instance_at('/items/9'))
    assert_equal(JSONSchemer::Redactor::REDACTED, redactor.instance_at('/items/1/x'))
    assert_nil(redactor.instance_at('/items/not-an-index'))
    assert_nil(redactor.instance_at('/items/01'))
  end

  def test_redactor_resolves_array_scalar_token
    data = ['keep', 'bad']
    redactor = JSONSchemer::Redactor.new(data, ['/1'])
    assert_equal(['keep', JSONSchemer::Redactor::REDACTED], redactor.instance)
    assert_equal(
      [JSONSchemer::Redactor::REDACTED],
      redactor.redact(['bad'])
    )
    out_of_range = JSONSchemer::Redactor.new(['keep'], ['/5'])
    assert_equal(['keep'], out_of_range.instance)

    missing_hash_key = JSONSchemer::Redactor.new({ 'present' => [] }, ['/missing/deep'])
    assert_equal({ 'present' => [] }, missing_hash_key.instance)
  end

  def test_detailed_with_redactor_when_no_matching_nested_results
    schemer = JSONSchemer.schema({
      'type' => 'object',
      'properties' => {
        'name' => { 'minLength' => 50 }
      }
    })
    secret = 'unique-secret-value'
    output = schemer.validate(
      { 'name' => secret },
      :output_format => 'detailed',
      :resolve_enumerators => true,
      :sensitive_paths => ['/name']
    )
    refute_includes(output.inspect, secret)
    assert_equal(false, output.fetch('valid'))
  end

  def test_redactor_redacts_hashes_arrays_scalars_and_ignored_types
    sensitive_hash = { 'password' => 'bad' }
    sensitive_array = ['bad']
    data = {
      'hash' => sensitive_hash,
      'array' => sensitive_array,
      'number' => 42,
      'symbol' => :bad,
      'boolean' => true,
      'null' => nil,
      'other' => 'keep'
    }
    redactor = JSONSchemer::Redactor.new(
      data,
      ['/hash', '/array', '/number', '/symbol', '/boolean', '/null']
    )

    assert_equal(
      {
        'hash' => JSONSchemer::Redactor::REDACTED,
        'array' => JSONSchemer::Redactor::REDACTED,
        'number' => JSONSchemer::Redactor::REDACTED,
        'symbol' => JSONSchemer::Redactor::REDACTED,
        'boolean' => JSONSchemer::Redactor::REDACTED,
        'null' => JSONSchemer::Redactor::REDACTED,
        'other' => 'keep'
      },
      redactor.instance
    )

    assert_equal({ 'password' => JSONSchemer::Redactor::REDACTED }, redactor.redact(sensitive_hash))
    assert_equal([JSONSchemer::Redactor::REDACTED], redactor.redact(sensitive_array))
    assert_equal(JSONSchemer::Redactor::REDACTED, redactor.redact(42))
    assert_equal(JSONSchemer::Redactor::REDACTED, redactor.redact(:bad))
    assert_equal(true, redactor.redact(true))
    assert_nil(redactor.redact(nil))
    assert_equal('keep', redactor.redact('keep'))
  end

  def test_redactor_redacts_equal_scalars_and_nested_containers
    redactor = JSONSchemer::Redactor.new({ 'secret' => 'bad' }, ['/secret'])

    assert_equal(
      [JSONSchemer::Redactor::REDACTED, { 'k' => [JSONSchemer::Redactor::REDACTED] }],
      redactor.redact(['bad', { 'k' => ['bad'] }])
    )
    assert(redactor.leaked?('prefix bad suffix'))
    refute(redactor.leaked?('nothing here'))
    refute(redactor.leaked?(42))
  end

  def test_redactor_marks_container_once_and_steps_through_scalars
    shared = { 'password' => 'bad' }
    data = { 'a' => shared, 'b' => shared, 'scalar' => 'x', 'nested' => { 'deep' => 'kept' } }
    redactor = JSONSchemer::Redactor.new(
      data,
      ['/a', '/b', '/scalar/traversal', '/nested/deep', '/nested/deep']
    )

    assert_equal(
      {
        'a' => JSONSchemer::Redactor::REDACTED,
        'b' => JSONSchemer::Redactor::REDACTED,
        'scalar' => 'x',
        'nested' => { 'deep' => JSONSchemer::Redactor::REDACTED }
      },
      redactor.instance
    )

    empty = JSONSchemer::Redactor.new({}, [])
    assert_equal({}, empty.instance)
    assert_nil(empty.instance_at('/anything'))
    assert_nil(empty.redact(nil))
  end

  def test_redactor_marks_shared_array_once
    shared = ['bad']
    data = { 'a' => shared, 'b' => shared }
    redactor = JSONSchemer::Redactor.new(data, ['/a', '/b'])
    assert_equal(
      { 'a' => JSONSchemer::Redactor::REDACTED, 'b' => JSONSchemer::Redactor::REDACTED },
      redactor.instance
    )
  end

  def test_redactor_instance_starts_as_scalar
    redactor = JSONSchemer::Redactor.new('bad', [''])
    assert_equal(JSONSchemer::Redactor::REDACTED, redactor.instance)
    assert_equal(JSONSchemer::Redactor::REDACTED, redactor.instance_at('/child'))
  end

  def test_redactor_identical_values_at_non_sensitive_locations_stay_visible
    data = {
      'tags' => ['dup', 'dup', 'keep'],
      'other' => { 'nested' => ['dup'] }
    }
    redactor = JSONSchemer::Redactor.new(data, ['/tags/0', '/tags/1'])

    assert_equal(
      {
        'tags' => [JSONSchemer::Redactor::REDACTED, JSONSchemer::Redactor::REDACTED, 'keep'],
        'other' => { 'nested' => ['dup'] }
      },
      redactor.instance
    )
  end

  def test_redactor_does_not_mutate_input
    redactor = JSONSchemer::Redactor.new(instance, sensitive_paths)

    assert_equal(instance, {
      'a/b~c' => { 'token' => 'tk' },
      'creds' => [{ 'password' => 'short', 'label' => 'ab' }],
      'name' => 'ab'
    })
    assert_equal(JSONSchemer::Redactor::REDACTED, redactor.instance_at('/creds'))
    assert_equal('short', instance.dig('creds', 0, 'password'))
  end

  def test_validation_without_sensitive_paths_is_unchanged
    failing_instance = {
      'a/b~c' => { 'token' => 'tk' },
      'creds' => [{ 'password' => 'bad', 'label' => 'ab' }],
      'name' => 'ab'
    }
    errors = schema.validate(failing_instance).to_a
    by_pointer = errors.to_h { |error| [error.fetch('data_pointer'), error] }
    assert_equal('bad', by_pointer.fetch('/creds/0/password').fetch('data'))
    assert_equal('tk', by_pointer.fetch('/a~1b~0c/token').fetch('data'))
  end

  def test_empty_sensitive_paths_is_equivalent_to_none
    errors = schema.validate(instance, :sensitive_paths => []).to_a
    assert_equal('ab', errors.find { |error| error.fetch('data_pointer') == '/creds/0/label' }.fetch('data'))
  end

  def test_nonexistent_sensitive_path_leaves_instance_visible
    schemer = JSONSchemer.schema({
      'type' => 'object',
      'properties' => { 'name' => { 'minLength' => 10 } }
    })
    errors = schemer.validate({ 'name' => 'ab' }, :sensitive_paths => ['/missing']).to_a
    assert_equal('ab', errors.first.fetch('data'))
  end

  def test_redaction_does_not_mutate_frozen_input
    schemer = JSONSchemer.schema({
      'properties' => { 'password' => { 'minLength' => 10 } }
    })
    data = { 'password' => 'ab' }.freeze
    output = schemer.validate(
      data,
      :output_format => 'basic',
      :resolve_enumerators => true,
      :sensitive_paths => ['/password']
    )
    assert_equal(false, output.fetch('valid'))
    assert_equal('ab', data.fetch('password'))
  end

  def test_valid_flag_output
    schemer = JSONSchemer.schema({ 'type' => 'string' })
    assert_equal({ 'valid' => true }, schemer.validate('hunter2secret', :output_format => 'flag', :sensitive_paths => ['']))
    assert_equal(true, schemer.valid?('hunter2secret', :sensitive_paths => ['']))
  end

  def test_valid_instance_annotations_are_redacted
    schemer = JSONSchemer.schema({ 'contains' => { 'const' => 'badsecret' } })
    output = schemer.validate(
      ['badsecret'],
      :output_format => 'verbose',
      :resolve_enumerators => true,
      :sensitive_paths => ['/0']
    )
    assert_redacted(output)
    refute_includes(output.inspect, 'badsecret')
  end

private

  def collect_instance_locations(output, locations = [])
    case output
    when Hash
      locations << output.fetch('instanceLocation') if output.key?('instanceLocation')
      output.each_value { |value| collect_instance_locations(value, locations) }
    when Array
      output.each { |value| collect_instance_locations(value, locations) }
    end
    locations
  end

  def with_translations(translations)
    file = Tempfile.create(['translations', '.yml'])
    file.write(YAML.dump(translations))
    file.flush
    I18n.load_path += [file.path]
    yield
  ensure
    I18n.load_path -= [file.path]
    File.unlink(file) rescue nil
  end
end
