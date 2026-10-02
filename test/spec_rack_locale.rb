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

    def full_locale_app(application = nil)
      application ||= lambda { |env| [200, {}, [I18n.locale.to_s]] }
      locales = ['zh-Hans', 'zh-Hant', 'en']
      Rack::Lint.new(
        Rack::Builder.new do
          use Rack::Locale, locales, default_locale: 'en'
          run application
        end
      )
    end

    def configured_full_locale_response(accept_languages)
      application = lambda { |env| [200, {}, [I18n.locale.to_s]] }
      app = Rack::Lint.new(
        Rack::Builder.new do
          use Rack::Locale, supported_locales: ['zh-Hans', 'zh-Hant', 'en'],
            default_locale: 'en'
          run application
        end
      )
      Rack::MockRequest.new(app).get('/', 'HTTP_ACCEPT_LANGUAGE' => accept_languages)
    end

    def full_locale_response(accept_languages, application = nil)
      request = Rack::MockRequest.new(full_locale_app(application))
      if accept_languages
        request.get('/', 'HTTP_ACCEPT_LANGUAGE' => accept_languages)
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
      specify 'matches exact, shortened, wildcard, and default tags' do
        response = full_locale_response('zh-Hant-TW;q=0.9,en;q=0.8')
        _(response.body).must_equal('zh-Hant')
        _(response.headers['Content-Language']).must_equal('zh-Hant')

        _(full_locale_response('zh;q=0.9,en;q=0.8').body).must_equal('zh-Hans')
        _(full_locale_response('fr').headers['Content-Language']).must_equal('en')
        _(full_locale_response('fr,*;q=0.1').body).must_equal('zh-Hans')
      end

      specify 'orders by quality and then header position for duplicates' do
        response = full_locale_response('zh-Hans;q=0.8, zh-Hant;q=0.8, en;q=0.9')
        _(response.body).must_equal('en')

        response = full_locale_response('zh-Hant;q=0.8, zh-Hans;q=0.8, en;q=0.8')
        _(response.body).must_equal('zh-Hant')

        response = full_locale_response('zh-Hant;q=0.9,zh-Hant;q=0.5')
        _(response.body).must_equal('zh-Hant')

        response = full_locale_response('*;q=0.5,en;q=0.5')
        _(response.body).must_equal('en')
      end

      specify 'keeps rejected tags out through wildcard and fallback' do
        response = full_locale_response('zh-Hant-TW;q=0.9,zh-Hant;q=0,en;q=0.8')
        _(response.body).must_equal('zh-Hans')

        response = full_locale_response('zh-Hant;q=0,*;q=0.1')
        _(response.body).must_equal('zh-Hans')

        response = full_locale_response('zh;q=0,*;q=0.1')
        _(response.body).must_equal('en')

        response = full_locale_response('zh-Hant-TW;q=0.9,*;q=0')
        _(response.body).must_equal('zh-Hant')

        response = full_locale_response('en;q=0,fr')
        _(response.status).must_equal(406)
      end

      specify 'ignores invalid weights and case-only variants' do
        response = full_locale_response('en;q=2,zh-Hans;q=0.9')
        _(response.body).must_equal('zh-Hans')

        response = full_locale_response('EN;q=0.9,en;q=0.8')
        _(response.body).must_equal('en')
      end

      specify 'accepts supported locales as keyword configuration' do
        _(configured_full_locale_response('zh-Hant').body).must_equal('zh-Hant')
      end

      specify 'prefers an exact primary language over its subtags' do
        application = lambda { |env| [200, {}, [I18n.locale.to_s]] }
        app = Rack::Lint.new(
          Rack::Builder.new do
            use Rack::Locale, ['zh', 'zh-Hans', 'zh-Hant', 'en'],
              default_locale: 'en'
            run application
          end
        )

        response = Rack::MockRequest.new(app).get('/', 'HTTP_ACCEPT_LANGUAGE' => 'zh')
        _(response.body).must_equal('zh')
      end

      specify 'does not change the global I18n language list' do
        I18n.locale = :en
        I18n.config.available_locales = [:global]
        full_locale_response('zh-Hant-TW')
        _(I18n.available_locales).must_equal([:global])
      end

      specify 'restores I18n locale after application exceptions' do
        I18n.locale = :es
        application = lambda { |_env| raise 'application failure' }

        _(proc { full_locale_response('zh-Hant-TW', application) }).must_raise(RuntimeError)
        _(I18n.locale).must_equal(:es)
      end
    end
  end
rescue LoadError
  STDERR.puts "WARN: Skipping Rack::Locale tests (i18n not installed)"
end
