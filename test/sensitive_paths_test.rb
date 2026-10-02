require 'test_helper'

class SensitivePathsTest < Minitest::Test
  SCHEMA = {
    'type' => 'object',
    'properties' => {
      'password' => { 'type' => 'string' },
      'a/b~c' => {
        'type' => 'object',
        'properties' => {
          'token' => { 'type' => 'string' }
        }
      },
      'secrets' => {
        'type' => 'array',
        'items' => { 'type' => 'string' }
      },
      'creds' => {
        'not' => { 'const' => { 'user' => 'admin', 'pw' => 'hunter2' } }
      },
      'name' => { 'type' => 'string' }
    }
  }

  INSTANCE = {
    'password' => 42,
    'a/b~c' => { 'token' => 123 },
    'secrets' => ['public', 7],
    'creds' => { 'user' => 'admin', 'pw' => 'hunter2' },
    'name' => 'visible'
  }

  PATHS = ['/password', '/a~1b~0c/token', '/secrets/1', '/creds']

  def schemer
    JSONSchemer.schema(SCHEMA)
  end

  def redacted_errors(output_format: 'classic', **options)
    options = { :output_format => output_format, :resolve_enumerators => true, :sensitive_paths => PATHS }
    schemer.validate(Marshal.load(Marshal.dump(INSTANCE)), **options)
  end

  def test_classic_redacts_exact_locations_and_parent_subtrees
    errors = redacted_errors.to_a

    by_pointer = errors.to_h { |error| [error.fetch('data_pointer'), error] }

    assert_equal('[REDACTED]', by_pointer.fetch('/password').fetch('data'))
    assert_equal('[REDACTED]', by_pointer.fetch('/a~1b~0c/token').fetch('data'))
    assert_equal('[REDACTED]', by_pointer.fetch('/secrets/1').fetch('data'))

    creds = by_pointer.fetch('/creds')
    assert_equal('[REDACTED]', creds.fetch('data'))
    assert_equal('not', creds.fetch('type'))
    assert_equal('value at `/creds` matches `not` schema', creds.fetch('error'))

    # locations, keywords, and schema-derived content are preserved
    assert_equal(
      ['/a~1b~0c/token', '/creds', '/password', '/secrets/1'],
      by_pointer.keys.sort
    )
    errors.each do |error|
      assert(error.key?('schema'))
      assert(error.key?('root_schema'))
      assert_equal(false, JSONSchemer.schema(SCHEMA).valid?(INSTANCE))
    end
  end

  def test_classic_output_contains_no_redacted_values
    output = JSON.generate(redacted_errors.to_a)
    refute_includes(output, '"data":123')
    refute_includes(output, '"data":42')
    refute_includes(output, '"data":7')
    refute_includes(output, '"data":{"user"')
  end

  def test_non_classic_outputs_keep_locations_keywords_and_validity
    %w[basic detailed verbose].each do |output_format|
      output = redacted_errors(:output_format => output_format)
      encoded = JSON.generate(output)

      assert_equal(false, output.fetch('valid'))
      assert_includes(encoded, '/password')
      assert_includes(encoded, '/a~1b~0c/token')
      assert_includes(encoded, '/secrets/1')
      assert_includes(encoded, '/creds')

      # these formats never carry instance data
      refute_includes(encoded, 'hunter2')
      refute_includes(encoded, '42')
    end
  end

  def test_unmatched_values_remain_visible
    errors = schemer.validate(
      Marshal.load(Marshal.dump(INSTANCE)),
      :sensitive_paths => ['/password']
    ).to_a

    secrets = errors.find { |error| error.fetch('data_pointer') == '/secrets/1' }
    assert_equal(7, secrets.fetch('data'))

    # a partially hidden parent keeps its untouched children
    parent = errors.find { |error| error.fetch('data_pointer') == '/creds' }
    # `/creds` is not sensitive here, so the parent object is reported as-is
    assert_equal(INSTANCE.fetch('creds'), parent.fetch('data')) if parent
  end

  def test_partial_object_redaction_keeps_siblings
    schema = {
      'type' => 'object',
      'properties' => {
        'credentials' => {
          'type' => 'object',
          'properties' => {
            'token' => { 'type' => 'string' },
            'note' => { 'type' => 'string' }
          }
        }
      }
    }
    instance = { 'credentials' => { 'token' => 9, 'note' => 'keep' } }

    errors = JSONSchemer.schema(schema).validate(
      instance,
      :sensitive_paths => ['/credentials/token']
    ).to_a

    parent = errors.find { |error| error.fetch('data_pointer') == '/credentials/token' }
    assert_equal('[REDACTED]', parent.fetch('data'))

    root_error = errors.find { |error| error.fetch('data_pointer') == '' }
    if root_error
      assert_equal(
        { 'credentials' => { 'token' => '[REDACTED]', 'note' => 'keep' } },
        root_error.fetch('data')
      )
    end
  end

  def test_parent_path_hides_descendant_error_values
    schema = {
      'properties' => {
        'credentials' => {
          'properties' => {
            'token' => { 'type' => 'string' },
            'nested' => {
              'properties' => { 'deep' => { 'type' => 'integer' } }
            }
          }
        }
      }
    }
    instance = {
      'credentials' => {
        'token' => 1,
        'nested' => { 'deep' => 'x' }
      }
    }

    errors = JSONSchemer.schema(schema).validate(
      instance,
      :sensitive_paths => ['/credentials']
    ).to_a

    assert(errors.any?)
    errors.each do |error|
      assert_equal('[REDACTED]', error.fetch('data'))
    end
  end

  def test_array_pointer_hides_whole_subtree
    schema = {
      'properties' => {
        'secrets' => {
          'type' => 'array',
          'items' => { 'type' => 'string' }
        }
      }
    }
    instance = { 'secrets' => [{ 'token' => 'a' }, 'b', 3] }

    errors = JSONSchemer.schema(schema).validate(
      instance,
      :sensitive_paths => ['/secrets']
    ).to_a

    errors.each do |error|
      assert_equal('[REDACTED]', error.fetch('data'))
    end
  end

  def test_pointer_to_nonexistent_location_is_tolerated
    errors = schemer.validate(
      Marshal.load(Marshal.dump(INSTANCE)),
      :sensitive_paths => ['/missing', '/password']
    ).to_a

    by_pointer = errors.to_h { |error| [error.fetch('data_pointer'), error] }
    assert_equal('[REDACTED]', by_pointer.fetch('/password').fetch('data'))
    assert_equal(7, by_pointer.fetch('/secrets/1').fetch('data'))
  end

  def test_sibling_array_items_remain_visible
    custom_schema = JSONSchemer.schema(
      { 'type' => 'object', 'check' => true },
      :keywords => {
        'check' => proc { |data, _schema, _pointer| [false, { 'saw' => data }] }
      }
    )
    instance = { 'secrets' => ['public', 7] }

    errors = custom_schema.validate(instance, :sensitive_paths => ['/secrets/1']).to_a
    errors.each do |error|
      assert_equal(['public', '[REDACTED]'], error.fetch('data').fetch('secrets'))
    end
  end

  def test_flag_output_keeps_working
    result = schemer.validate(
      Marshal.load(Marshal.dump(INSTANCE)),
      :output_format => 'flag',
      :sensitive_paths => PATHS
    )
    assert_equal({ 'valid' => false }, result)
  end

  def test_valid_instance_stays_valid_with_redaction
    schema = {
      'properties' => {
        'password' => { 'type' => 'string' }
      }
    }
    instance = { 'password' => 'secret' }
    assert_equal(
      { 'valid' => true },
      JSONSchemer.schema(schema).validate(
        instance,
        :output_format => 'flag',
        :sensitive_paths => ['/password']
      )
    )
    assert_equal({ 'password' => 'secret' }, instance)
  end

  def test_original_instance_is_not_mutated
    instance = { 'password' => 42, 'secrets' => ['a', 7] }
    original = Marshal.load(Marshal.dump(instance))

    schemer.validate(instance, :sensitive_paths => ['/password', '/secrets/1'])

    assert_equal(original, instance)
  end

  def test_output_without_sensitive_paths_is_unchanged
    instance = Marshal.load(Marshal.dump(INSTANCE))
    errors = schemer.validate(instance).to_a

    by_pointer = errors.to_h { |error| [error.fetch('data_pointer'), error] }
    assert_equal(42, by_pointer.fetch('/password').fetch('data'))
    assert_equal(123, by_pointer.fetch('/a~1b~0c/token').fetch('data'))
    assert_equal(7, by_pointer.fetch('/secrets/1').fetch('data'))
    assert_equal(INSTANCE.fetch('creds'), by_pointer.fetch('/creds').fetch('data'))
  end

  def test_validation_uses_real_data
    schema = {
      'properties' => {
        'token' => {
          'allOf' => [
            { 'type' => 'string' },
            { 'minLength' => 2 }
          ]
        }
      }
    }
    sensitive = JSONSchemer.schema(schema)

    # valid string: redaction must not turn the real value into '[REDACTED]'
    assert(sensitive.valid?({ 'token' => 'abc' }, :sensitive_paths => ['/token']))

    # invalid string still fails on the real value
    refute(sensitive.valid?({ 'token' => 'x' }, :sensitive_paths => ['/token']))

    # wrong type fails as well
    refute(sensitive.valid?({ 'token' => 9 }, :sensitive_paths => ['/token']))
  end

  def test_root_pointer_redacts_everything
    errors = JSONSchemer.schema(SCHEMA).validate(
      Marshal.load(Marshal.dump(INSTANCE)),
      :sensitive_paths => ['']
    ).to_a

    refute_empty(errors)
    assert(errors.all? { |error| error.fetch('data') == '[REDACTED]' })
  end

  def test_custom_keyword_details_are_redacted
    custom_schema = JSONSchemer.schema(
      { 'type' => 'object', 'secret_check' => true },
      :keywords => {
        'secret_check' => proc { |data, _schema, _pointer| [false, { 'saw' => data }] }
      }
    )
    instance = { 'token' => 'supersecret', 'name' => 'ok' }

    redacted = custom_schema.validate(instance, :sensitive_paths => ['/token']).to_a
    redacted.each do |error|
      assert_equal('[REDACTED]', error.fetch('details').fetch('result'))
      assert_equal('secret_check', error.fetch('details').fetch('keyword'))
      assert_equal('[REDACTED]', error.fetch('data').fetch('token'))
      assert_equal('ok', error.fetch('data').fetch('name'))
    end
    refute_includes(JSON.generate(redacted), 'supersecret')

    unredacted = custom_schema.validate(Marshal.load(Marshal.dump(instance))).to_a
    assert_includes(JSON.generate(unredacted), 'supersecret')
  end

  def test_builtin_details_remain_untouched
    schema = {
      'properties' => {
        'password' => { 'type' => 'string' }
      },
      'required' => ['password', 'name']
    }
    errors = JSONSchemer.schema(schema).validate(
      { 'other' => true },
      :sensitive_paths => ['/password']
    ).to_a

    required = errors.find { |error| error.fetch('type') == 'required' }
    assert_equal({ 'missing_keys' => ['password', 'name'] }, required.fetch('details'))
  end

  def test_x_error_text_is_not_modified
    schema = {
      'properties' => {
        'password' => {
          'type' => 'string',
          'x-error' => 'got %{instance} at %{instanceLocation}'
        }
      }
    }
    errors = JSONSchemer.schema(schema).validate(
      { 'password' => 42 },
      :sensitive_paths => ['/password']
    ).to_a

    error = errors.find { |entry| entry.fetch('data_pointer') == '/password' }
    assert_equal('got 42 at /password', error.fetch('error'))
    assert(error.fetch('x-error'))
    assert_equal('[REDACTED]', error.fetch('data'))
  end

  def test_invalid_sensitive_pointer_raises
    error = assert_raises(JSONSchemer::InvalidSensitivePath) do
      schemer.validate(INSTANCE, :sensitive_paths => ['password'])
    end
    assert_equal('password', error.message)
  end

  def test_empty_sensitive_paths_is_a_noop
    instance = Marshal.load(Marshal.dump(INSTANCE))
    errors = schemer.validate(instance, :sensitive_paths => []).to_a

    by_pointer = errors.to_h { |error| [error.fetch('data_pointer'), error] }
    assert_equal(42, by_pointer.fetch('/password').fetch('data'))
  end
end
