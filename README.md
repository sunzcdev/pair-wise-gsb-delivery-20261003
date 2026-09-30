# Warden

[![Build Status](https://github.com/wardencommunity/warden/workflows/Tests/badge.svg?branch=master)](https://github.com/wardencommunity/warden/actions)

## Getting Started

Please see the [Warden Wiki](https://github.com/wardencommunity/warden/wiki) for overview documentation.

## All-of strategy groups

By default Warden runs strategies as a chain: the first strategy that
succeeds logs the user in. An _all-of_ group instead requires every member
strategy to succeed, and requires them all to authenticate the same subject.

Register a group with `Warden::Strategies.all_of`, giving it a label, the
member strategy labels, and a block that compares two authenticated subjects.
The comparison block is mandatory because two Ruby objects are not guaranteed
to be `==` just because they represent the same identity:

```ruby
Warden::Strategies.all_of(:password_and_totp, :password, :totp) do |first, other|
  first.id == other.id
end
```

The group behaves like any other strategy and can be passed to
`authenticate` or configured via `default_strategies`:

```ruby
env['warden'].authenticate(:password_and_totp)
```

Only when every member succeeds and the comparison block confirms a single
identity is the user written into the scope. If a later factor fails (or a
different subject is authenticated), no user is stored, no half
authenticated session remains, and the `after_authentication` hook is not
run. Listing the strategies separately (`authenticate(:password, :totp)`)
keeps the original first-success-wins behavior.

## Maintainers

* Daniel Neighman (hassox)
* José Valim (josevalim)
* Justin Smestad (jsmestad)
* Whitney Smestad (whithub)

[A list of all contributors is available on Github.](https://github.com/hassox/warden/contributors)

## LICENSE

See `LICENSE` file.
