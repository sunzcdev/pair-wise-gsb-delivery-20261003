Add ``--config-audit`` and ``virtualenv.audit_via_cli`` to explain how the config file, the environment variables and the
command line resolve each option. The report names the layer that provided the value the creation reads, shows what each
layer offered, and tells an unset variable apart from one set to an empty string and from a value that cannot be parsed,
naming the source. The config file also accepts the historical hyphenated key spelling again and labels it in the report.
