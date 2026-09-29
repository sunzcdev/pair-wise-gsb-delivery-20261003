# Rack::Session

Session management implementation for Rack.

[![Development Status](https://github.com/rack/rack-session/workflows/Test/badge.svg)](https://github.com/rack/rack-session/actions?workflow=Test)

## Usage

In your `config.ru`:

``` ruby
# config.ru

require 'rack/session'
use Rack::Session::Cookie,
  :domain => 'mywebsite.com',
  :path => '/',
  :expire_after => 3600*24,
  :secret => '**unique secret key**'
```

Usage follows the standard outlined by `rack.session`, i.e.:

``` ruby
class MyApp
  def call(env)
    session = env['rack.session']

    # Set some state:
    session[:key] = "value"
  end
end
```

### Encrypted sessions

Encrypted session cookies are authenticated with the configured cookie name and
path. Set a unique `application_id` for each application so cookies cannot be
exchanged when multiple applications are accidentally configured with the same
secret:

``` ruby
use Rack::Session::Cookie,
  secrets: File.binread("config/session_secret.bin"),
  application_id: "billing",
  key: "billing.session",
  path: "/"
```

New cookies use the current authenticated format. During a limited migration,
older encrypted cookies can be accepted by explicitly setting `legacy_mode` to
`:v1`, `:v2`, or `true` for both formats. Remove the option after clients have
received replacement cookies; legacy cookies cannot carry the new application
and path binding.

### Compatibility

`rack-session` code used to be part of Rack, but it was extracted in Rack v3 to this gem. The v1 release of this gem is compatible with Rack v2, and the v2 release of this gem is compatible with Rack v3+. That means you can add `gem "rack-session"` to your application and it will be compatible with all versions of Rack.

## Contributing

We welcome contributions to this project.

1.  Fork it.
2.  Create your feature branch (`git checkout -b my-new-feature`).
3.  Commit your changes (`git commit -am 'Add some feature'`).
4.  Push to the branch (`git push origin my-new-feature`).
5.  Create new Pull Request.
