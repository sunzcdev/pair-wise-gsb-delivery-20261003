# Security Policy

We deeply appreciate any effort to discover and disclose security vulnerabilities responsibly.

For any security bug or issue with the RubyGems client, Bundler, or RubyGems.org service, please email security@rubygems.org with details about the problem or submit a report using [HackerOne](https://hackerone.com/rubygems). A malicious or compromised gem published on RubyGems.org can be reported the same way.

For additional information about RubyGems security, please see https://rubygems.org/pages/security.

## Out of scope for RubyGems and Bundler

The following are not treated as vulnerabilities. A technically correct report may still be fixed in public as a regular bug or hardening change.

- Code you chose to run, whether a gem you install or the `Gemfile` of a repository you run `bundle` in. A gem can ship a native extension whose build runs arbitrary code as you, and a `Gemfile` is Ruby code, so anything a crafted gem or repository does within your privileges, such as writing or deleting files or path traversal through gem metadata, adds no capability, whether or not the gem declares extensions.
- A gem source or mirror you chose to trust. Reports that assume the configured source is malicious or compromised are out of scope, and so is gem metadata that rubygems.org rejects at push time, since it can only arrive from a local file or a gem server you chose to trust.
- Your own environment and settings. This covers attacks that require write access to files RubyGems or Bundler already load on your behalf, such as plugins, `.gemrc` or `.bundle/config`, and behavior after you explicitly disable a protection such as TLS certificate verification.
- Issues already fixed on the `master` branch. Please check it before reporting.
