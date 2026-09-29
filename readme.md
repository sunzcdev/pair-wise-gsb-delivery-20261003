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

### Compatibility

`rack-session` code used to be part of Rack, but it was extracted in Rack v3 to this gem. The v1 release of this gem is compatible with Rack v2, and the v2 release of this gem is compatible with Rack v3+. That means you can add `gem "rack-session"` to your application and it will be compatible with all versions of Rack.

### Session Revocation

`Rack::Session::Cookie` keeps the entire session in the encrypted cookie, so there is no server-side record to delete when a user wants to "log out of all devices". The `:revocation_check` option provides a small revocation interface: the cookie carries a subject identifier and the epoch at which it was issued, and your callback reports the subject's current epoch. If the two don't match, the session is rejected.

``` ruby
use Rack::Session::Cookie,
  :secret => '**unique secret key**',
  # Your own datastore lookup; no database client is bundled:
  :revocation_check => ->(subject) { SessionEpochs.fetch(subject) }

# In your application, enroll a session by storing its subject:
env['rack.session'][Rack::Session::Cookie::REVOCATION_SUBJECT_KEY] = user.id

# To log the user out of all devices, bump their epoch:
SessionEpochs.increment(user.id)
```

The behavior is specified as follows:

- Anonymous sessions (no subject stored) are never checked and the callback is not invoked.
- If the subject changes during a request, the new subject is checked when the session is written.
- If the callback raises or returns `nil` (unavailable), the middleware fails closed: the session is rejected on read and not saved on write.
- Within a single request, the callback is invoked at most once per subject; the result is memoized in the request environment.
- The epoch is always re-stamped from the callback when the session is written, never copied from the incoming cookie, so a cookie issued before an epoch bump cannot be renewed back into validity.

## Contributing

We welcome contributions to this project.

1.  Fork it.
2.  Create your feature branch (`git checkout -b my-new-feature`).
3.  Commit your changes (`git commit -am 'Add some feature'`).
4.  Push to the branch (`git push origin my-new-feature`).
5.  Create new Pull Request.
