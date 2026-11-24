from typing import cast
from unittest import TestCase

from pyinfra.api import Config, Host, Inventory, State
from pyinfra.api.arguments import (
    UNSET,
    AllArguments,
    get_connector_arguments,
    pop_global_arguments,
)


class TestOperationKwargs(TestCase):
    def test_get_from_config(self):
        config = Config(SUDO="config-value")
        inventory = Inventory((("somehost",), {}))

        state = State(config=config, inventory=inventory)
        host = inventory.get_host("somehost")

        kwargs, _ = pop_global_arguments(state, host, {})
        assert kwargs.get("_sudo") == "config-value"

    def test_get_from_host(self):
        config = Config(SUDO="config-value")
        inventory = Inventory(([("somehost", {"_sudo": True})], {}))

        state = State(config=config, inventory=inventory)
        host = inventory.get_host("somehost")

        kwargs, _ = pop_global_arguments(state, host, {})
        assert kwargs.get("_sudo") is True

    def test_get_from_state_deploy_kwargs(self):
        config = Config(SUDO="config-value")
        inventory = Inventory(([("somehost", {"_sudo": False})], {}))
        somehost = inventory.get_host("somehost")
        assert isinstance(somehost, Host)

        state = State(config=config, inventory=inventory)
        host = inventory.get_host("somehost")
        somehost.current_deploy_kwargs = cast(AllArguments, {"_sudo": True})

        kwargs, keys = pop_global_arguments(state, host, {})
        assert kwargs.get("_sudo") is True

    def test_get_from_kwargs(self):
        config = Config(SUDO="config-value")
        inventory = Inventory(([("somehost", {"_sudo": False})], {}))
        somehost = inventory.get_host("somehost")
        assert isinstance(somehost, Host)

        state = State(config=config, inventory=inventory)
        somehost.current_deploy_kwargs = cast(
            AllArguments,
            {
                "_sudo": False,
                "_sudo_user": "deploy-kwarg-user",
            },
        )

        kwargs, keys = pop_global_arguments(state, somehost, {"_sudo": True})
        assert kwargs.get("_sudo") is True
        assert kwargs.get("_sudo_user") == "deploy-kwarg-user"
        assert "_sudo" in keys

    def test_unset_is_distinct_from_none_for_connector_arguments(self):
        config = Config(SUDO_USER="config-user")
        inventory = Inventory((("somehost",), {}))
        state = State(config=config, inventory=inventory)
        host = inventory.get_host("somehost")
        host.current_deploy_kwargs = cast(AllArguments, {"_sudo_user": "outer-user"})

        kwargs, _ = pop_global_arguments(
            state,
            host,
            {"_sudo_user": UNSET, "_chdir": None},
        )
        connector_kwargs = get_connector_arguments(kwargs)

        # The engine sees a resolved default, but the connector scope records the
        # explicit unset and therefore omits the argument.
        assert kwargs["_sudo_user"] == "config-user"
        assert "_sudo_user" in kwargs.unset_keys
        assert "_sudo_user" not in connector_kwargs

        # Explicit None remains a real connector value.
        assert kwargs["_chdir"] is None
        assert "_chdir" not in kwargs.unset_keys
        assert connector_kwargs["_chdir"] is None

    def test_env_override_can_remove_inherited_key(self):
        config = Config(ENV={"INHERITED": "from-config"})
        inventory = Inventory((("somehost",), {}))
        state = State(config=config, inventory=inventory)
        host = inventory.get_host("somehost")
        host.current_deploy_kwargs = cast(
            AllArguments,
            {"_env": {"INHERITED": "outer", "NEIGHBOR": "keep"}},
        )

        kwargs, _ = pop_global_arguments(
            state,
            host,
            {"_env": {"INHERITED": None}},
        )

        assert kwargs["_env"] == {"NEIGHBOR": "keep"}
        assert kwargs["_env"].unset_keys == frozenset({"INHERITED"})
        assert get_connector_arguments(kwargs)["_env"] == {"NEIGHBOR": "keep"}

        # Resolving another nested layer must not let config.ENV re-add the key.
        host.current_deploy_kwargs = kwargs
        nested_kwargs, _ = pop_global_arguments(state, host, {})
        assert nested_kwargs["_env"] == {"NEIGHBOR": "keep"}
