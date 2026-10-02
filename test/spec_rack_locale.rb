# frozen_string_literal: true

require 'minitest/autorun'
require 'minitest/hooks'
require 'rack/mock'

begin
  require './lib/rack/contrib/locale'

  describe "Rack::Locale" do
    include Minitest::Hooks

    before do
      # Set the locales that will be used at various points in the tests
      I18n.config.available_locales = [:en, :dk, :'en-gb', :es, :zh]
      I18n.default_locale = :en
    end

    def app
      @app ||= Rack::Lint.new(
        Rack::Builder.new do
          use Rack::Locale
          run lambda { |env| [ 200, {}, [ I18n.locale.to_s ] ] }
        end
      )
    end

    def response_with_languages(accept_languages)
      if accept_languages
        Rack::MockRequest.new(app).get('/', { 'HTTP_ACCEPT_LANGUAGE' => accept_languages } )
      else
        Rack::MockRequest.new(app).get('/', {})
      end
    end

    def full_language_app(available_locales = full_available_locales, default_locale: 'en', app: nil)
      inner_app = app || lambda { |env| [ 200, {}, [ I18n.locale.to_s ] ] }
      Rack::Lint.new(
        Rack::Locale.new(
          inner_app,
          available_locales,
          default_locale: default_locale
        )
      )
    end

    def full_available_locales
      %w(en zh-Hans zh-Hant)
    end

    def full_language_response(accept_languages, **options)
      request = Rack::MockRequest.new(full_language_app(**options))
      if accept_languages
        request.get('/', { 'HTTP_ACCEPT_LANGUAGE' => accept_languages })
      else
        request.get('/', {})
      end
    end

    def enforce_available_locales(enforce)
      default_enforce = I18n.enforce_available_locales
      I18n.enforce_available_locales = enforce
      yield
    ensure
      I18n.enforce_available_locales = default_enforce
    end

    specify 'should use I18n.default_locale if no languages are requested' do
      I18n.default_locale = :zh
      _(response_with_languages(nil).body).must_equal('zh')
    end

    specify 'should treat an empty qvalue as 1.0' do
      _(response_with_languages('en,es;q=0.95').body).must_equal('en')
    end

    specify 'should set the Content-Language response header' do
      headers = response_with_languages('de;q=0.7,dk;q=0.9').headers
      _(headers['Content-Language']).must_equal('dk')
    end

    specify 'should pick the language with the highest qvalue' do
      _(response_with_languages('en;q=0.9,es;q=0.95').body).must_equal('es')
    end

    specify 'should ignore spaces between comma separated field values' do
      _(response_with_languages('en;q=0.9, es;q=0.95').body).must_equal('es')
    end

    specify 'should ignore spaces within quality value' do
      _(response_with_languages('en; q=0.9,es; q=0.95').body).must_equal('es')
    end

    specify 'should retain full language codes' do
      _(response_with_languages('en-gb,en-us;q=0.95;en').body).must_equal('en-gb')
    end

    specify 'should match languages with variants' do
      _(response_with_languages('pt;Q=0.9,es-CL').body).must_equal('es')
    end

    specify 'should match languages with variants case insensitively' do
      _(response_with_languages('pt;Q=0.9,ES-CL').body).must_equal('es')
    end

    specify 'should skip * if it is followed by other languages' do
      _(response_with_languages('*,dk;q=0.5').body).must_equal('dk')
    end

    specify 'should use default locale if there is only *' do
      _(response_with_languages('*').body).must_equal('en')
    end

    specify 'should ignore languages with q=0' do
      _(response_with_languages('dk;q=0').body).must_equal(I18n.default_locale.to_s)
    end

    specify 'should handle Q=' do
      _(response_with_languages('en;Q=0.9,es;Q=0.95').body).must_equal('es')
    end

    specify 'should reset the I18n locale after the response' do
      I18n.locale = :es
      response_with_languages('en,de;q=0.8')
      _(I18n.locale).must_equal(:es)
    end

    specify 'should pick the available language' do
      enforce_available_locales(true) do
        _(response_with_languages('ch,en;q=0.9,es;q=0.95').body).must_equal('es')
      end
    end

    specify 'should match languages case insensitively' do
      enforce_available_locales(true) do
        _(response_with_languages('EN;q=0.9,ES;q=0.95').body).must_equal('es')
      end
    end

    specify 'should use default_locale if there is no matching language while enforcing available_locales' do
      I18n.default_locale = :zh
      enforce_available_locales(true) do
        _(response_with_languages('ja').body).must_equal('zh')
      end
    end

    specify 'when not enforce should pick the language with the highest qvalue' do
      enforce_available_locales(false) do
        _(response_with_languages('ch,en;q=0.9').body).must_equal('ch')
      end
    end

    describe 'full language tag negotiation' do
      specify 'shortens a requested language tag before falling back' do
        response = full_language_response('zh-Hant-TW')
        _(response.body).must_equal('zh-Hant')
        _(response.headers['Content-Language']).must_equal('zh-Hant')
      end

      specify 'prefers exact matches over shorter prefix matches' do
        response = full_language_response('zh-Hans-HK,zh-Hant;q=0.9')
        _(response.body).must_equal('zh-Hans')
        _(response.headers['Content-Language']).must_equal('zh-Hans')
      end

      specify 'orders duplicate ranges by weight and then header position' do
        response = full_language_response('zh-Hans;q=0.8,zh-Hant;q=0.8,zh-Hans;q=0.9')
        _(response.body).must_equal('zh-Hans')

        response = full_language_response('zh-Hant;q=0.8,zh-Hans;q=0.8')
        _(response.body).must_equal('zh-Hant')
      end

      specify 'prefers a higher weighted prefix over a lower weighted exact match' do
        response = full_language_response('zh-Hans;q=0.8,zh-Hant-TW;q=0.9')
        _(response.body).must_equal('zh-Hant')
      end

      specify 'does not let a wildcard select an explicitly rejected language' do
        response = full_language_response('zh-Hant;q=0,zh-Hans;q=0,*;q=0.5')
        _(response.body).must_equal('en')
        _(response.headers['Content-Language']).must_equal('en')
      end

      specify 'allows an explicit language to override a broader rejection' do
        response = full_language_response('zh;q=0,zh-Hant;q=0.9')
        _(response.body).must_equal('zh-Hant')
      end

      specify 'does not shorten into a rejected language' do
        response = full_language_response('zh;q=0,zh-Hant-TW')
        _(response.body).must_equal('en')

        response = full_language_response('zh-Hant;q=0,zh-Hant-TW')
        _(response.body).must_equal('en')
      end

      specify 'uses a wildcard before the default language' do
        app_options = { default_locale: 'zh-Hans' }
        response = full_language_response('zh-Hans;q=0,zh-Hant;q=0,*;q=0.5', **app_options)
        _(response.body).must_equal('en')
      end

      specify 'returns the default language when no requested language matches' do
        response = full_language_response('ja,fr;q=0.8')
        _(response.body).must_equal('en')
        _(response.headers['Content-Language']).must_equal('en')
      end

      specify 'returns 406 when the default language is rejected' do
        response = full_language_response('en;q=0,ja')
        _(response.status).must_equal(406)
        _(response.headers['Content-Language']).must_be_nil
      end

      specify 'returns 406 for a rejected wildcard' do
        response = full_language_response('*;q=0')
        _(response.status).must_equal(406)
        _(response.headers['Content-Language']).must_be_nil
      end

      specify 'does not change I18n state when returning 406' do
        I18n.locale = :es
        enforce_available_locales(true) do
          full_language_response('*;q=0')
          _(I18n.locale).must_equal(:es)
          _(I18n.enforce_available_locales).must_equal(true)
        end
      end

      specify 'ignores ranges with invalid weights' do
        response = full_language_response('zh-Hant;q=invalid,zh-Hans;q=1.2,*;q=0.5')
        _(response.body).must_equal('en')
      end

      specify 'matches language tags case insensitively without creating another locale' do
        response = full_language_response('ZH-hant-tw')
        _(response.body).must_equal('zh-Hant')
        _(response.headers['Content-Language']).must_equal('zh-Hant')
      end

      specify 'does not change the I18n available locales' do
        original_available_locales = I18n.available_locales
        full_language_response('zh-Hant-TW')
        _(I18n.available_locales).must_equal(original_available_locales)
      end

      specify 'uses configured locales even when I18n enforces available locales' do
        enforce_available_locales(true) do
          response = full_language_response('zh-Hant-TW')
          _(response.body).must_equal('zh-Hant')
          _(I18n.enforce_available_locales).must_equal(true)
        end
      end

      specify 'restores the previous I18n locale when the app raises' do
        I18n.locale = :es
        failing_app = lambda { |_env| raise RuntimeError, 'boom' }
        request = Rack::MockRequest.new(
          full_language_app(full_available_locales, app: failing_app)
        )

        _(-> {
          request.get('/', { 'HTTP_ACCEPT_LANGUAGE' => 'zh-Hant-TW' })
        }).must_raise(RuntimeError)
        _(I18n.locale).must_equal(:es)
      end
    end
  end
rescue LoadError
  STDERR.puts "WARN: Skipping Rack::Locale tests (i18n not installed)"
end
