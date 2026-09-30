# Warden

[![Build Status](https://github.com/wardencommunity/warden/workflows/Tests/badge.svg?branch=master)](https://github.com/wardencommunity/warden/actions)

## Getting Started

Please see the [Warden Wiki](https://github.com/wardencommunity/warden/wiki) for overview documentation.

## Reauthentication / step-up authentication

Being logged in is not always sufficient. Sensitive actions (changing
payment details, for example) may require that the user authenticated
recently, or through a stronger method. Warden records authentication
*evidence* per scope &mdash; the strategy that succeeded and when it
completed &mdash; in the session. Restoring the user from the session never
refreshes this timestamp; only a successful authentication strategy does.

Passively inspect the evidence with `authenticated?` (or its alias
`reauthenticated?`):

```ruby
env['warden'].authenticated?(:max_age => 300)
env['warden'].authenticated?(:methods => :otp, :max_age => 300)
env['warden'].authentication_method  # => :password
env['warden'].last_authentication_at # => Time
```

For sensitive entry points use `authenticate_recently!`. It attempts the
listed strategies so a fresh, stronger authentication can happen in the same
request; if the requirements cannot be met it throws `:warden` (rendered
through the failure app), while the ordinary, still-valid login is kept:

```ruby
env['warden'].authenticate_recently!(:otp, :max_age => 300)
```

The clock is injectable with the `:clock` option (any object responding to
`#now`), or globally via `Warden::Manager.new(app, :clock => clock)`, which
keeps tests deterministic.

## Maintainers

* Daniel Neighman (hassox)
* José Valim (josevalim)
* Justin Smestad (jsmestad)
* Whitney Smestad (whithub)

[A list of all contributors is available on Github.](https://github.com/hassox/warden/contributors)

## LICENSE

See `LICENSE` file.
