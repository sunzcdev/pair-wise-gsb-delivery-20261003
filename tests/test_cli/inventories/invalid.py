# Variables names with a leading underscore are ignored
_hosts = ["host-1", "host-2", "host-3"]

# Dictionaries are not supported
dict_hosts = {
    "host-1": "hostname",
}

# Generators are not supported
generator_hosts = (host for host in ["host-1", "host-2"])

# https://github.com/pyinfra-dev/pyinfra/issues/662
issue_662 = ["a", "b", ({"foo": "bar"})]
