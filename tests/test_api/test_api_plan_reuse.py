from unittest import TestCase
from unittest.mock import patch

from pyinfra.api import Config, Inventory, State
from pyinfra.api.facts import FactBase, fact_cache_key, get_fact
from pyinfra.api.identity import HOST_IDENTITY_DATA_KEY, resolve_host_identity
from pyinfra.api.operation import add_op
from pyinfra.api.operations import run_ops
from pyinfra.api.plan_reuse import ConfirmedStep, PlanReuseStore
from pyinfra.api.state import StateStage
from pyinfra.connectors.util import CommandOutput, OutputLine
from pyinfra.operations import server


class ServiceState(FactBase):
    """A fact whose value is whatever the fake target currently reports."""

    def command(self):
        return "read service state"


class PeerValue(FactBase):
    """A second fact, used to check confirmed values are served from plan reuse."""

    def command(self):
        return "read peer value"


def run_plan(inventory, plan_file=None):
    """Build a state for ``inventory``, seeding plan reuse from ``plan_file`` (if any)."""

    state = State(inventory, Config())
    state.current_stage = StateStage.Prepare
    state.record_facts = True  # as --save-plan-reuse would

    store = PlanReuseStore.from_dict(plan_file)
    snapshot = state.set_plan_reuse(store)
    return state, store, snapshot


class TestHostIdentity(TestCase):
    def test_same_name_different_ssh_target_is_a_different_identity(self):
        old = Inventory(([("same", {"ssh_hostname": "initial"})], {}))
        new = Inventory(([("same", {"ssh_hostname": "replacement"})], {}))

        old_identity = resolve_host_identity(old.get_host("same"))
        new_identity = resolve_host_identity(new.get_host("same"))

        assert old_identity.name == new_identity.name == "same"
        assert old_identity.describe() == "SSHConnector(ssh_hostname=initial)"
        assert new_identity.describe() == "SSHConnector(ssh_hostname=replacement)"
        assert old_identity.fingerprint != new_identity.fingerprint
        assert not old_identity.matches(new_identity)

    def test_same_target_matches_regardless_of_display_name(self):
        left = Inventory(([("web", {"ssh_hostname": "10.0.0.5"})], {}))
        right = Inventory(([("web-renamed", {"ssh_hostname": "10.0.0.5"})], {}))

        assert resolve_host_identity(left.get_host("web")).matches(
            resolve_host_identity(right.get_host("web-renamed")),
        )

    def test_ssh_port_is_part_of_the_identity(self):
        left = Inventory(([("web", {"ssh_hostname": "10.0.0.5", "ssh_port": 22})], {}))
        right = Inventory(([("web", {"ssh_hostname": "10.0.0.5", "ssh_port": 2222})], {}))

        assert not resolve_host_identity(left.get_host("web")).matches(
            resolve_host_identity(right.get_host("web")),
        )

    def test_group_membership_and_group_data_are_not_identity(self):
        base = Inventory(([("web", {"ssh_hostname": "10.0.0.5"})], {}))
        annotated = Inventory(
            ([("web", {"ssh_hostname": "10.0.0.5"})], {}),
            extra_group=(["web"], {"note": "audited"}),
        )

        assert resolve_host_identity(base.get_host("web")).matches(
            resolve_host_identity(annotated.get_host("web")),
        )
        # The group label did land on the host, it just is not part of its identity.
        assert "extra_group" in annotated.get_host("web").groups
        assert annotated.get_host("web").data.note == "audited"
        assert resolve_host_identity(base.get_host("web")).target_data == {
            "ssh_hostname": "10.0.0.5",
        }

    def test_explicit_host_identity_data_wins_over_connector_target(self):
        inventory = Inventory(
            (
                [
                    ("web", {"ssh_hostname": "10.0.0.5", HOST_IDENTITY_DATA_KEY: "rack-slot-4"}),
                ],
                {},
            ),
        )
        identity = resolve_host_identity(inventory.get_host("web"))

        assert identity.source == "host_identity"
        assert identity.target_data == {"explicit": "rack-slot-4"}

    def test_identity_round_trips_through_serialisation(self):
        inventory = Inventory(([("web", {"ssh_hostname": "10.0.0.5"})], {}))
        identity = resolve_host_identity(inventory.get_host("web"))

        restored = type(identity).from_dict(identity.as_dict())
        assert restored == identity
        assert restored.matches(identity)


class TestPlanReuseAcceptance(TestCase):
    """
    The scenario from the request: a two-host deploy completes; on reload only the first
    host's target changes, while the second host merely gains a harmless group label.
    """

    def _inventory(self, first_target, second_groups=False):
        groups = {}
        if second_groups:
            groups = {"second_group": (["peer"], {"note": "harmless"})}
        return Inventory(
            (
                [
                    ("first", {"ssh_hostname": first_target}),
                    ("peer", {"ssh_hostname": "same-peer"}),
                ],
                {},
            ),
            **groups,
        )

    def _run_step_on_both(self, state):
        for host in state.inventory:
            state.activate_host(host)
            host.connected = True
        with (
            patch.object(
                state.inventory.get_host("first"),
                "run_shell_command",
                return_value=(True, CommandOutput([OutputLine("stdout", "initial")])),
            ),
            patch.object(
                state.inventory.get_host("peer"),
                "run_shell_command",
                return_value=(True, CommandOutput([OutputLine("stdout", "same-peer")])),
            ),
        ):
            add_op(state, server.shell, "apply", host=list(state.inventory))
            run_ops(state)

    def test_first_host_refuses_reuse_second_host_keeps_its_record(self):
        # Original deploy: two hosts, one step, both complete.
        original_state, _, _ = run_plan(self._inventory("initial"))
        self._run_step_on_both(original_state)

        plan = PlanReuseStore().to_dict(original_state)

        # Reload: first is re-pointed, peer only gains a group label.
        reload_state, _, snapshot = run_plan(self._inventory("replacement", True), plan)

        first = reload_state.inventory.get_host("first")
        peer = reload_state.inventory.get_host("peer")

        # First host: same display name, different target -> refuses the old result.
        assert snapshot.resolved["first"].reason == "identity_changed"
        assert not snapshot.resolved["first"].reused
        assert snapshot.facts_for(first) == {}

        # Second host: identity unchanged by the group label -> keeps its own record.
        assert snapshot.resolved["peer"].reason == "match"
        assert snapshot.resolved["peer"].reused
        report = snapshot.as_dict()
        assert report["first"]["reason"] == "identity_changed"
        assert report["first"]["previous_identity"]["target"] == {"ssh_hostname": "initial"}
        assert report["first"]["identity"]["target"] == {"ssh_hostname": "replacement"}
        assert report["peer"]["reused"] is True
        # The confirmed step was bound to the peer identity, so it still resolves there.
        step_hash = next(iter(plan["records"]["peer"]["steps"]))
        assert snapshot.confirmed_step(peer, step_hash) is not None
        assert snapshot.confirmed_step(first, step_hash) is None

    def test_refused_host_borrows_no_facts_but_matching_host_does(self):
        # Seed a plan with a confirmed fact for both hosts.
        plan = {
            "version": 1,
            "records": {
                "first": {
                    "identity": {
                        "name": "first",
                        "connector": "SSHConnector",
                        "target": {"ssh_hostname": "initial"},
                        "source": "connector",
                    },
                    "facts": {"x": "old-first"},
                    "steps": {},
                },
                "peer": {
                    "identity": {
                        "name": "peer",
                        "connector": "SSHConnector",
                        "target": {"ssh_hostname": "same-peer"},
                        "source": "connector",
                    },
                    "facts": {"x": "old-peer"},
                    "steps": {},
                },
            },
        }

        state, _, snapshot = run_plan(self._inventory("replacement", True), plan)
        first = state.inventory.get_host("first")
        peer = state.inventory.get_host("peer")

        assert snapshot.facts_for(first) == {}
        assert snapshot.facts_for(peer) == {"x": "old-peer"}

        # The refused host gathers its fact fresh instead of reusing the old value.
        fresh = []

        def fake_run(command, **kwargs):
            fresh.append(str(command))
            return True, CommandOutput([OutputLine("stdout", "fresh-first")])

        first.connected = True
        with patch.object(first, "run_shell_command", side_effect=fake_run):
            value = get_fact(state, first, ServiceState)
        assert value == "fresh-first"
        assert fresh == ["read service state"]
        recorded = state.confirmed_facts["first"]
        assert list(recorded.values()) == ["fresh-first"]
        cache_key = next(iter(recorded))
        assert cache_key.startswith("tests.test_api.test_api_plan_reuse.ServiceState:")

    def test_second_host_still_resolves_its_confirmed_step(self):
        plan = {
            "version": 1,
            "records": {
                "peer": {
                    "identity": {
                        "name": "peer",
                        "connector": "SSHConnector",
                        "target": {"ssh_hostname": "same-peer"},
                        "source": "connector",
                    },
                    "facts": {},
                    "steps": {"step-hash": {"success": True, "did_change": True, "commands": 1}},
                },
            },
        }

        state, _, snapshot = run_plan(self._inventory("replacement", True), plan)

        peer = state.inventory.get_host("peer")
        confirmed = snapshot.confirmed_step(peer, "step-hash")

        assert confirmed == ConfirmedStep(success=True, did_change=True, commands=1)
        assert snapshot.resolved["first"].reason == "new_host"
        assert snapshot.confirmed_step(state.inventory.get_host("first"), "step-hash") is None

    def test_group_data_change_does_not_invalidate_confirmed_facts(self):
        inventory = Inventory(
            ([("peer", {"ssh_hostname": "same-peer"})], {}),
            extra_group=(["peer"], {"note": "audited"}),
        )
        state, _, snapshot = run_plan(inventory)

        assert snapshot.resolved["peer"].reused is False  # no prior record
        assert snapshot.resolved["peer"].reason == "new_host"
        plain = Inventory(([("peer", {"ssh_hostname": "same-peer"})], {}))
        assert snapshot.resolved["peer"].identity.matches(
            resolve_host_identity(plain.get_host("peer")),
        )


class TestPlanReuseStore(TestCase):
    def test_missing_payload_yields_disabled_store(self):
        store = PlanReuseStore.from_dict(None)
        assert store.enabled is False
        assert store.records == {}

    def test_unknown_version_is_ignored(self):
        store = PlanReuseStore.from_dict({"version": 99, "records": {}})
        assert store.enabled is False

    def test_confirming_run_advances_the_stored_identity(self):
        # Run against "initial", then reload against "replacement": the new record is
        # bound to the new target, so a third load against "replacement" may reuse it.
        initial = Inventory(([("first", {"ssh_hostname": "initial"})], {}))
        state, _, _ = run_plan(initial)
        for host in state.inventory:
            state.activate_host(host)
            host.connected = True
        with patch.object(
            state.inventory.get_host("first"),
            "run_shell_command",
            return_value=(True, CommandOutput([OutputLine("stdout", "initial")])),
        ):
            add_op(state, server.shell, "apply", host=list(state.inventory))
            run_ops(state)

        plan = PlanReuseStore().to_dict(state)
        assert plan["records"]["first"]["identity"]["target"] == {"ssh_hostname": "initial"}

        replacement = Inventory(([("first", {"ssh_hostname": "replacement"})], {}))
        reload_state, _, snapshot = run_plan(replacement, plan)
        assert snapshot.resolved["first"].reason == "identity_changed"

        # Confirm the reloaded plan; the record now describes the replacement machine.
        updated = PlanReuseStore.from_dict(plan).to_dict(reload_state)
        assert updated["records"]["first"]["identity"]["target"] == {
            "ssh_hostname": "replacement",
        }

        # And a second reload against the same replacement reuses it.
        again = Inventory(([("first", {"ssh_hostname": "replacement"})], {}))
        _, _, third_snapshot = run_plan(again, updated)
        assert third_snapshot.resolved["first"].reused is True


class TestPlanReuseExecution(TestCase):
    """
    Execute-phase behaviour: a confirmed step is skipped for an identity-matched host and
    replayed for a re-pointed host, and facts confirmed for the matching host are served
    without touching the target during the next plan.
    """

    def _fake_inventory(self, first_identity, extra_group=False):
        groups = {}
        if extra_group:
            groups = {"ops_extra": (["@fake/peer"], {"note": "harmless"})}
        return Inventory(
            (
                [
                    (
                        "@fake/first",
                        {
                            "host_identity": first_identity,
                            "fake_delay": 0,
                            "fake_delay_jitter": 0,
                        },
                    ),
                    (
                        "@fake/peer",
                        {
                            "host_identity": "machine-b",
                            "fake_delay": 0,
                            "fake_delay_jitter": 0,
                        },
                    ),
                ],
                {},
            ),
            **groups,
        )

    def _run(self, inventory, plan=None):
        state = State(inventory, Config())
        state.current_stage = StateStage.Prepare
        state.record_facts = True
        store = PlanReuseStore.from_dict(plan)
        state.set_plan_reuse(store)
        for host in state.inventory:
            state.activate_host(host)
            host.connected = True
        add_op(state, server.shell, "apply", host=list(state.inventory))
        return state, store

    def test_step_is_replayed_for_changed_target_and_reused_for_labelled_peer(self):
        state, _ = self._run(self._fake_inventory("machine-a"))
        run_ops(state)
        plan = PlanReuseStore().to_dict(state)

        reloaded, _ = self._run(self._fake_inventory("machine-a-replaced", extra_group=True), plan)
        run_ops(reloaded)

        first = reloaded.inventory.get_host("@fake/first")
        peer = reloaded.inventory.get_host("@fake/peer")
        first_meta = reloaded.ops[first][next(iter(reloaded.ops[first]))].operation_meta
        peer_meta = reloaded.ops[peer][next(iter(reloaded.ops[peer]))].operation_meta

        # The re-pointed host actually ran its command again.
        assert first.connector.executed_commands == ["apply"]
        assert first_meta.reused is False

        # The peer kept its identity through the extra group label, so its confirmed step
        # was reused and nothing was sent to it.
        assert peer.connector.executed_commands == []
        assert peer_meta.reused is True
        assert peer_meta.did_succeed() is True

        # Reused steps still show up as success in the results.
        results = reloaded.get_results_for_host(peer)
        assert results.success_ops == 1

    def test_confirmed_fact_served_without_running_the_command(self):
        state, _ = self._run(self._fake_inventory("machine-a"))
        # Confirm a fact on the matching peer.
        peer = state.inventory.get_host("@fake/peer")
        peer.connector.data["fake_responses"] = {"read peer value": "old-value"}
        assert get_fact(state, peer, PeerValue) == "old-value"

        plan = PlanReuseStore().to_dict(state)

        # Reload with a harmless group label on the peer; the confirmed fact is served
        # straight from the snapshot.
        reloaded, _ = self._run(self._fake_inventory("machine-a-replaced", extra_group=True), plan)
        new_peer = reloaded.inventory.get_host("@fake/peer")
        new_peer.connector.executed_commands.clear()

        assert get_fact(reloaded, new_peer, PeerValue) == "old-value"
        assert new_peer.connector.executed_commands == []

        # A re-pointed host never borrows the old value: it runs the read itself.
        first = reloaded.inventory.get_host("@fake/first")
        first.connector.data["fake_responses"] = {"read peer value": "fresh-value"}
        assert get_fact(reloaded, first, PeerValue) == "fresh-value"
        assert first.connector.executed_commands == ["read peer value"]


class TestFactCacheKey(TestCase):
    def test_key_includes_fact_arguments(self):
        base = fact_cache_key("server.Users", {"self": "", "user": "alice"}, {})
        other = fact_cache_key("server.Users", {"self": "", "user": "bob"}, {})

        assert base != other
        assert base.startswith("server.Users:")

    def test_key_includes_connector_arguments(self):
        plain = fact_cache_key("server.Users", {}, {})
        sudo = fact_cache_key("server.Users", {}, {"_sudo": True})

        assert plain != sudo

    def test_key_is_stable_for_equal_arguments(self):
        assert fact_cache_key("server.Users", {"user": "alice"}, {}) == fact_cache_key(
            "server.Users",
            {"user": "alice"},
            {},
        )
