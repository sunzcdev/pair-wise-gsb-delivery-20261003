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

### Server-side session expiry

The `:expire_after` option only controls the client-side cookie expiry, which the client is free to ignore. `Rack::Session::Cookie` can additionally enforce session lifetimes on the server side:

``` ruby
use Rack::Session::Cookie,
  :secret => '**unique secret key**',
  # Maximum session lifetime, measured from when the session was created:
  :absolute_lifetime => 3600*24,
  # Maximum idle time since the last activity:
  :idle_timeout => 3600,
  # Read-only requests only re-issue the cookie once this many seconds have
  # elapsed since the last activity update (default: half of :idle_timeout):
  :renewal_threshold => 300
```

The creation and last activity times are stored inside the authenticated (encrypted/signed) cookie content and validated using an injectable clock (`:clock`, defaults to `->{ Time.now }`). Because the timestamps are authenticated, a client cannot extend a session by keeping the cookie past its expiry or by rewriting the cookie's expiry attributes. Renewing a session updates the last activity time, but never the creation time.

Cookies written before these options were enabled do not contain time metadata; they are accepted and migrated (stamped with the current time) the next time the session is written, so their absolute lifetime is measured from the time of migration. If the server clock moves backwards, elapsed times are clamped to zero: sessions are neither expired nor renewed until the clock catches up.

### Compatibility

`rack-session` code used to be part of Rack, but it was extracted in Rack v3 to this gem. The v1 release of this gem is compatible with Rack v2, and the v2 release of this gem is compatible with Rack v3+. That means you can add `gem "rack-session"` to your application and it will be compatible with all versions of Rack.

## Contributing

We welcome contributions to this project.

1.  Fork it.
2.  Create your feature branch (`git checkout -b my-new-feature`).
3.  Commit your changes (`git commit -am 'Add some feature'`).
4.  Push to the branch (`git push origin my-new-feature`).
5.  Create new Pull Request.
