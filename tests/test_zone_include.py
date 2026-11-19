# Copyright (C) Dnspython Contributors, see LICENSE for text of ISC license

"""Tests for $INCLUDE confinement, source resolution, and diagnostics."""

import os
import subprocess

import pytest

import dns.exception
import dns.rdatatype
import dns.zone
import dns.zonefile

HEADER = """$TTL 300
$ORIGIN example.
@ IN SOA ns1 hostmaster 1 2 3 4 5
@ IN NS ns1
"""


def _write(directory, name, body):
    path = directory / name
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(body, encoding="utf-8")
    return path


def _directory_link(link, target):
    """Make *link* a directory link to *target*, or skip the test.

    Windows does not permit symbolic links without a privilege which CI does
    not have, so a junction is used there instead.  A junction resolves like a
    symbolic link for the purposes of this test, i.e. ``realpath()`` follows
    it to the real target.
    """
    try:
        os.symlink(target, link, target_is_directory=True)
        return
    except (OSError, NotImplementedError):
        pass
    if os.name != "nt":
        pytest.skip("cannot create directory links on this platform")
    result = subprocess.run(
        ["cmd", "/c", "mklink", "/J", str(link), str(target)],
        capture_output=True,
        text=True,
    )
    if result.returncode != 0:
        pytest.skip(f"cannot create a directory junction: {result.stderr.strip()}")


def _file_link(link, target):
    try:
        os.symlink(target, link)
    except (OSError, NotImplementedError):
        pytest.skip("cannot create file links on this platform")


def _rrs(zone, rtype="A"):
    """Return the rdatasets of type *rtype* in *zone*, keyed by name text."""
    found = {}
    wanted = dns.rdatatype.from_text(rtype)
    for name, node in zone.nodes.items():
        for rdataset in node:
            if rdataset.rdtype == wanted:
                found[str(name)] = rdataset
    return found


def _load(path, **kwargs):
    kwargs.setdefault("allow_include", True)
    kwargs.setdefault("relativize", True)
    return dns.zone.from_file(path, "example.", **kwargs)


def _error(path, **kwargs):
    with pytest.raises(dns.zonefile.IncludeError) as info:
        _load(path, **kwargs)
    return info.value


def test_relative_include_is_relative_to_the_including_file(tmp_path):
    _write(tmp_path, "sub/deeper.zone", "deeper 300 IN A 10.0.0.9\n")
    _write(
        tmp_path, "sub/inner.zone", "$INCLUDE deeper.zone\ninner 300 IN A 10.0.0.8\n"
    )
    main = _write(tmp_path, "main.zone", HEADER + "$INCLUDE sub/inner.zone\n")
    zone = _load(main)
    assert set(_rrs(zone)) == {"deeper", "inner"}


def test_include_does_not_use_the_process_working_directory(tmp_path, monkeypatch):
    # A relative include inside sub/ must be found in sub/, not in the
    # current working directory, which we make somewhere else entirely.
    elsewhere = tmp_path / "elsewhere"
    elsewhere.mkdir()
    _write(elsewhere, "shared.zone", "shared 300 IN A 10.0.0.1\n")
    _write(tmp_path, "sub/inner.zone", "$INCLUDE shared.zone\n")
    main = _write(tmp_path, "main.zone", HEADER + "$INCLUDE sub/inner.zone\n")
    monkeypatch.chdir(elsewhere)
    with pytest.raises(dns.zonefile.IncludeError):
        _load(main)


def test_default_confinement_is_the_directory_of_the_file(tmp_path):
    # With no include_directories, includes are confined to the directory of
    # the file being read.
    inside = tmp_path / "inside"
    inside.mkdir()
    _write(inside, "ok.zone", "ok 300 IN A 10.0.0.2\n")
    main = _write(inside, "main.zone", HEADER + "$INCLUDE ok.zone\n")
    zone = _load(main)
    assert "ok" in _rrs(zone)


def test_default_confinement_blocks_escape_from_the_file_directory(tmp_path):
    inside = tmp_path / "inside"
    inside.mkdir()
    _write(tmp_path, "outside/secret.zone", "x 300 IN A 10.0.0.1\n")
    main = _write(inside, "main.zone", HEADER + "$INCLUDE ../outside/secret.zone\n")
    error = _error(main)
    assert isinstance(error, dns.zonefile.IncludeNotAllowed)


def test_unrestricted_resolver_allows_any_file(tmp_path):
    inside = tmp_path / "inside"
    inside.mkdir()
    _write(tmp_path, "outside/secret.zone", "x 300 IN A 10.0.0.1\n")
    main = _write(inside, "main.zone", HEADER + "$INCLUDE ../outside/secret.zone\n")
    zone = _load(main, include_resolver=dns.zonefile.IncludeResolver())
    assert "x" in _rrs(zone)


def test_sandbox_allows_files_inside(tmp_path):
    inside = tmp_path / "inside"
    _write(inside, "ok.zone", "ok 300 IN A 10.0.0.2\n")
    main = _write(inside, "main.zone", HEADER + "$INCLUDE ok.zone\n")
    zone = _load(main, include_directories=[inside])
    assert "ok" in _rrs(zone)


def test_sandbox_blocks_parent_directory_escape(tmp_path):
    inside = tmp_path / "inside"
    _write(tmp_path, "outside/secret.zone", "x 300 IN A 10.0.0.1\n")
    main = _write(inside, "main.zone", HEADER + "$INCLUDE ../outside/secret.zone\n")
    error = _error(main, include_directories=[inside])
    assert isinstance(error, dns.zonefile.IncludeNotAllowed)


def test_sandbox_blocks_absolute_path_outside(tmp_path):
    inside = tmp_path / "inside"
    outside = _write(tmp_path, "outside/secret.zone", "x 300 IN A 10.0.0.1\n")
    main = _write(inside, "main.zone", HEADER + f'$INCLUDE "{outside}"\n')
    error = _error(main, include_directories=[inside])
    assert isinstance(error, dns.zonefile.IncludeNotAllowed)


def test_sandbox_allows_absolute_path_inside(tmp_path):
    inside = tmp_path / "inside"
    target = _write(inside, "ok.zone", "ok 300 IN A 10.0.0.2\n")
    main = _write(inside, "main.zone", HEADER + f'$INCLUDE "{target}"\n')
    zone = _load(main, include_directories=[inside])
    assert "ok" in _rrs(zone)


def test_sandbox_blocks_a_directory_link(tmp_path):
    inside = tmp_path / "inside"
    outside = tmp_path / "outside"
    _write(outside, "secret.zone", "x 300 IN A 10.0.0.1\n")
    inside.mkdir()
    _directory_link(inside / "jump", outside)
    main = _write(inside, "main.zone", HEADER + "$INCLUDE jump/secret.zone\n")
    error = _error(main, include_directories=[inside])
    assert isinstance(error, dns.zonefile.IncludeNotAllowed)


def test_sandbox_blocks_a_file_link(tmp_path):
    inside = tmp_path / "inside"
    target = _write(tmp_path, "outside/secret.zone", "x 300 IN A 10.0.0.1\n")
    inside.mkdir()
    _file_link(inside / "link.zone", target)
    main = _write(inside, "main.zone", HEADER + "$INCLUDE link.zone\n")
    error = _error(main, include_directories=[inside])
    assert isinstance(error, dns.zonefile.IncludeNotAllowed)


def test_empty_directory_list_prohibits_includes(tmp_path):
    _write(tmp_path, "ok.zone", "ok 300 IN A 10.0.0.2\n")
    main = _write(tmp_path, "main.zone", HEADER + "$INCLUDE ok.zone\n")
    error = _error(main, include_directories=[])
    assert isinstance(error, dns.zonefile.IncludeNotAllowed)


def test_sandbox_blocks_a_name_escaping_through_a_link(tmp_path):
    # A file whose canonical location is outside must be rejected even if the
    # name written in the directive is entirely inside the directory.
    inside = tmp_path / "inside"
    outside = tmp_path / "outside"
    _write(outside, "secret.zone", "x 300 IN A 10.0.0.1\n")
    inside.mkdir()
    _directory_link(inside / "jump", outside)
    main = _write(inside, "main.zone", HEADER + "$INCLUDE jump\n")
    error = _error(main, include_directories=[inside])
    assert isinstance(error, dns.zonefile.IncludeNotAllowed)


def test_include_cycle_is_detected(tmp_path):
    _write(tmp_path, "a.zone", "$INCLUDE b.zone\n")
    _write(tmp_path, "b.zone", "$INCLUDE a.zone\n")
    main = _write(tmp_path, "main.zone", HEADER + "$INCLUDE a.zone\n")
    error = _error(main)
    assert isinstance(error, dns.zonefile.IncludeCycle)


def test_self_include_is_detected(tmp_path):
    main = _write(tmp_path, "main.zone", HEADER + "$INCLUDE main.zone\n")
    error = _error(main)
    assert isinstance(error, dns.zonefile.IncludeCycle)


def test_same_file_may_be_included_twice(tmp_path):
    _write(tmp_path, "twice.zone", "twice 300 IN A 10.0.0.5\n")
    main = _write(
        tmp_path, "main.zone", HEADER + "$INCLUDE twice.zone\n$INCLUDE twice.zone\n"
    )
    zone = _load(main)
    assert "twice" in _rrs(zone)


def test_same_file_may_be_included_in_two_branches(tmp_path):
    _write(tmp_path, "shared.zone", "shared 300 IN A 10.0.0.6\n")
    _write(tmp_path, "branch1.zone", "$INCLUDE shared.zone\n")
    _write(tmp_path, "branch2.zone", "$INCLUDE shared.zone\n")
    main = _write(
        tmp_path,
        "main.zone",
        HEADER + "$INCLUDE branch1.zone\n$INCLUDE branch2.zone\n",
    )
    zone = _load(main)
    assert "shared" in _rrs(zone)


def test_depth_limit(tmp_path):
    for i in range(5):
        _write(tmp_path, f"d{i}.zone", f"$INCLUDE d{i + 1}.zone\n")
    _write(tmp_path, "d5.zone", "leaf 300 IN A 10.0.0.1\n")
    main = _write(tmp_path, "main.zone", HEADER + "$INCLUDE d0.zone\n")
    error = _error(main, max_include_depth=3)
    assert isinstance(error, dns.zonefile.IncludeTooDeep)


def test_depth_zero_prohibits_includes(tmp_path):
    _write(tmp_path, "ok.zone", "ok 300 IN A 10.0.0.2\n")
    main = _write(tmp_path, "main.zone", HEADER + "$INCLUDE ok.zone\n")
    error = _error(main, max_include_depth=0)
    assert isinstance(error, dns.zonefile.IncludeTooDeep)


def test_depth_allows_a_flat_include(tmp_path):
    _write(tmp_path, "ok.zone", "ok 300 IN A 10.0.0.2\n")
    main = _write(tmp_path, "main.zone", HEADER + "$INCLUDE ok.zone\n")
    zone = _load(main, max_include_depth=1)
    assert "ok" in _rrs(zone)


def test_depth_does_not_confuse_a_sibling_with_the_stack(tmp_path):
    # b.zone and c.zone both include d.zone, but neither is being read when
    # the other includes it, so this is legal even at a small depth.
    _write(tmp_path, "d.zone", "d 300 IN A 10.0.0.4\n")
    _write(tmp_path, "b.zone", "$INCLUDE d.zone\n")
    _write(tmp_path, "c.zone", "$INCLUDE d.zone\n")
    main = _write(tmp_path, "main.zone", HEADER + "$INCLUDE b.zone\n$INCLUDE c.zone\n")
    zone = _load(main, max_include_depth=2)
    assert "d" in _rrs(zone)


def test_cycle_diagnostic_has_the_whole_chain(tmp_path):
    _write(tmp_path, "a.zone", "$INCLUDE b.zone\n")
    _write(tmp_path, "b.zone", "$INCLUDE a.zone\n")
    main = _write(tmp_path, "main.zone", HEADER + "$INCLUDE a.zone\n")
    error = _error(main)
    text = str(error)
    # The chain is main.zone:5 -> a.zone:1 -> b.zone:1 -> a.zone.
    assert f"{main}:{HEADER.count(chr(10)) + 1}" in text
    assert "a.zone:1" in text
    assert "b.zone:1" in text
    assert error.chain[0].endswith(f"main.zone:{HEADER.count(chr(10)) + 1}")
    assert os.path.realpath(error.chain[-1]).endswith("a.zone")


def test_include_error_has_a_reason_and_chain(tmp_path):
    _write(tmp_path, "mid.zone", "$INCLUDE missing.zone\n")
    main = _write(tmp_path, "main.zone", HEADER + "$INCLUDE mid.zone\n")
    error = _error(main)
    assert isinstance(error, dns.zonefile.IncludeError)
    assert "missing.zone" in error.reason
    assert error.chain[0].endswith(f"main.zone:{HEADER.count(chr(10)) + 1}")
    assert error.chain[1].endswith("mid.zone:1")


def test_syntax_error_in_an_included_file_reports_the_chain(tmp_path):
    _write(tmp_path, "bad.zone", "bad 300 IN\n")
    _write(tmp_path, "mid.zone", "$INCLUDE bad.zone\n")
    main = _write(tmp_path, "main.zone", HEADER + "$INCLUDE mid.zone\n")
    with pytest.raises(dns.exception.SyntaxError) as info:
        _load(main)
    text = str(info.value)
    assert "bad.zone:" in text
    assert "mid.zone:1" in text
    assert f"{main}:{HEADER.count(chr(10)) + 1}" in text


def test_valid_multiple_includes_do_not_look_like_a_cycle(tmp_path):
    # The same file, included twice in different places, and a file which is
    # included from a file which was itself already finished, are both fine.
    _write(tmp_path, "leaf.zone", "leaf 300 IN A 10.0.0.7\n")
    _write(tmp_path, "first.zone", "$INCLUDE leaf.zone\nfirst 300 IN A 10.0.0.1\n")
    _write(tmp_path, "second.zone", "$INCLUDE leaf.zone\nsecond 300 IN A 10.0.0.2\n")
    main = _write(
        tmp_path,
        "main.zone",
        HEADER
        + "$INCLUDE first.zone\n"
        + "$INCLUDE leaf.zone\n"
        + "$INCLUDE second.zone\n"
        + "$INCLUDE first.zone\n",
    )
    zone = _load(main)
    assert {"first", "second", "leaf"} <= set(_rrs(zone))


def test_origin_and_ttl_are_restored_after_an_include(tmp_path):
    _write(
        tmp_path,
        "inc.zone",
        "$TTL 111\n$ORIGIN sub.example.\nin1 A 10.0.0.1\n"
        "$ORIGIN other.example.\nin2 A 10.0.0.2\n",
    )
    main = _write(
        tmp_path,
        "main.zone",
        "$TTL 300\n$ORIGIN example.\n"
        "@ IN SOA ns1 hostmaster 1 2 3 4 5\n@ IN NS ns1\n"
        "$ORIGIN pre.example.\n"
        "$INCLUDE inc.zone ex.example.\n"
        "after A 10.0.0.3\n",
    )
    zone = _load(main, relativize=False)
    assert _rrs(zone)["in1.sub.example."].ttl == 111
    assert _rrs(zone)["in2.other.example."].ttl == 111
    # The $ORIGIN stated inside the include does not outlive it, and the
    # $ORIGIN stated just before it (pre.example.) is back in effect.
    assert "after.pre.example." in _rrs(zone)
    assert _rrs(zone)["after.pre.example."].ttl == 300


def test_include_argument_origin_applies_inside_the_include(tmp_path):
    _write(tmp_path, "inc.zone", "in1 A 10.0.0.1\n")
    main = _write(
        tmp_path,
        "main.zone",
        HEADER + "$INCLUDE inc.zone arg.example.\n" + "after A 10.0.0.3\n",
    )
    zone = _load(main, relativize=False)
    assert "in1.arg.example." in _rrs(zone)
    assert "after.example." in _rrs(zone)


def test_include_resolver_instance_can_be_supplied(tmp_path):
    inside = tmp_path / "inside"
    _write(inside, "ok.zone", "ok 300 IN A 10.0.0.2\n")
    main = _write(inside, "main.zone", HEADER + "$INCLUDE ok.zone\n")
    resolver = dns.zonefile.IncludeResolver([inside], max_depth=2)
    zone = _load(main, include_resolver=resolver)
    assert "ok" in _rrs(zone)


def test_include_resolver_rejects_a_negative_depth():
    with pytest.raises(ValueError):
        dns.zonefile.IncludeResolver(["."], max_depth=-1)


def test_include_resolver_properties(tmp_path):
    inside = tmp_path / "inside"
    _write(inside, "ok.zone", "ok 300 IN A 10.0.0.2\n")
    resolver = dns.zonefile.IncludeResolver([inside], max_depth=4)
    assert resolver.directories == (os.path.realpath(inside),)
    assert resolver.max_depth == 4
    assert resolver.depth == 0
    assert resolver.root is None
    resolver.set_root(inside / "main.zone")
    assert resolver.root is not None
    assert resolver.root.filename == os.path.realpath(inside / "main.zone")
    assert resolver.current_path() == resolver.root.filename
    assert resolver.include_chain() == []
    # Entering an include makes it the current file and adds a chain element.
    origin = dns.zonefile.IncludeOrigin("<string>", 3, resolver.root.filename)
    resolver.enter("ok.zone", origin)
    assert resolver.depth == 1
    assert resolver.current_path() == os.path.realpath(inside / "ok.zone")
    assert resolver.include_chain() == ["<string>:3"]
    resolver.leave()
    assert resolver.depth == 0
    assert resolver.current_path() == resolver.root.filename


def test_include_resolver_without_a_root_uses_the_working_directory(
    tmp_path, monkeypatch
):
    _write(tmp_path, "ok.zone", "ok 300 IN A 10.0.0.2\n")
    monkeypatch.chdir(tmp_path)
    resolver = dns.zonefile.IncludeResolver()
    assert resolver.current_path() is None
    source = resolver.resolve("ok.zone")
    assert source.filename == os.path.realpath(tmp_path / "ok.zone")


def test_include_resolver_resolve_does_not_open(tmp_path):
    inside = tmp_path / "inside"
    _write(inside, "ok.zone", "ok 300 IN A 10.0.0.2\n")
    resolver = dns.zonefile.IncludeResolver([inside])
    source = resolver.resolve(
        "ok.zone", dns.zonefile.IncludeOrigin("<string>", 1, str(inside / "main.zone"))
    )
    assert source.filename == os.path.realpath(inside / "ok.zone")
    assert source.directory == os.path.realpath(inside)
    # Resolving does not consume or open the file: entering twice in a row
    # works, and leaving without an open file is harmless.
    source2, file = resolver.enter(
        "ok.zone", dns.zonefile.IncludeOrigin("<string>", 1, str(inside / "main.zone"))
    )
    assert source2 == source
    resolver.leave()
    resolver.leave()
    file.close()


def test_from_text_relative_include_uses_the_filename(tmp_path):
    inside = tmp_path / "inside"
    _write(inside, "ok.zone", "ok 300 IN A 10.0.0.2\n")
    main = inside / "main.zone"
    text = HEADER + "$INCLUDE ok.zone\n"
    zone = dns.zone.from_text(
        text, "example.", relativize=True, allow_include=True, filename=str(main)
    )
    assert "ok" in _rrs(zone)


def test_from_text_without_a_filename_uses_the_working_directory(
    tmp_path, monkeypatch
):
    # Input which is not a file has no directory to confine to, so a relative
    # include is resolved against the working directory and is not confined.
    _write(tmp_path, "ok.zone", "ok 300 IN A 10.0.0.2\n")
    monkeypatch.chdir(tmp_path)
    text = HEADER + "$INCLUDE ok.zone\n"
    zone = dns.zone.from_text(text, "example.", relativize=True, allow_include=True)
    assert "ok" in _rrs(zone)


def test_a_resolver_root_anchors_string_input(tmp_path):
    # A resolver given an explicit root lets string input (which has no path of
    # its own) resolve relative includes against that root's directory.
    inside = tmp_path / "inside"
    _write(inside, "ok.zone", "ok 300 IN A 10.0.0.2\n")
    resolver = dns.zonefile.IncludeResolver([inside])
    resolver.set_root(inside / "anchor.zone")
    text = HEADER + "$INCLUDE ok.zone\n"
    zone = dns.zone.from_text(
        text,
        "example.",
        relativize=True,
        allow_include=True,
        include_resolver=resolver,
    )
    assert "ok" in _rrs(zone)


def test_from_text_filename_confines_includes(tmp_path):
    inside = tmp_path / "inside"
    inside.mkdir()
    _write(tmp_path, "outside/secret.zone", "x 300 IN A 10.0.0.1\n")
    main = inside / "main.zone"
    text = HEADER + "$INCLUDE ../outside/secret.zone\n"
    with pytest.raises(dns.zonefile.IncludeNotAllowed):
        dns.zone.from_text(
            text,
            "example.",
            relativize=True,
            allow_include=True,
            filename=str(main),
        )


def test_original_entry_points_still_work(tmp_path, monkeypatch):
    # Including with none of the new arguments behaves as it always did.
    include = os.path.join(os.path.dirname(__file__), "ttl_include.text")
    monkeypatch.chdir(os.path.dirname(include))
    text = (
        "$ORIGIN example.\n@ IN SOA x y 1 2 3 4 5\n@ IN NS a\n"
        f'$INCLUDE "{include}"\n'
    )
    zone = dns.zone.from_text(text, "example.", relativize=True, allow_include=True)
    assert "included" in _rrs(zone)


def test_include_of_an_empty_filename_is_an_error(tmp_path):
    main = _write(tmp_path, "main.zone", HEADER + '$INCLUDE ""\n')
    error = _error(main)
    assert "empty" in error.reason.lower()


def test_include_from_an_open_file_object(tmp_path, monkeypatch):
    # A file object has no name, so includes are resolved against the working
    # directory and are unconfined, as they always were.
    _write(tmp_path, "ok.zone", "ok 300 IN A 10.0.0.2\n")
    main = _write(tmp_path, "main.zone", HEADER + "$INCLUDE ok.zone\n")
    monkeypatch.chdir(tmp_path)
    with open(main, encoding="utf-8") as fp:
        zone = dns.zone.from_file(fp, "example.", relativize=True, allow_include=True)
    assert "ok" in _rrs(zone)


def test_include_filename_may_be_quoted(tmp_path):
    _write(tmp_path, "q.zone", "q 300 IN A 10.0.0.1\n")
    main = _write(tmp_path, "main.zone", HEADER + '$INCLUDE "q.zone"\n')
    zone = _load(main)
    assert "q" in _rrs(zone)


def test_missing_filename_in_include_is_a_syntax_error(tmp_path):
    main = _write(tmp_path, "main.zone", HEADER + "$INCLUDE\n")
    with pytest.raises(dns.exception.SyntaxError):
        _load(main)


def test_included_files_are_closed_after_an_error(tmp_path):
    # A failure deep in an include must not leak open files.
    for i in range(3):
        _write(tmp_path, f"d{i}.zone", f"$INCLUDE d{i + 1}.zone\n")
    _write(tmp_path, "d3.zone", "bad 300 IN\n")
    main = _write(tmp_path, "main.zone", HEADER + "$INCLUDE d0.zone\n")
    resolver = dns.zonefile.IncludeResolver()
    with pytest.raises(dns.exception.SyntaxError):
        _load(main, include_resolver=resolver)
    # Every entered frame was closed and popped.
    assert resolver.depth == 0


def test_included_file_is_closed_on_success(tmp_path):
    _write(tmp_path, "inc.zone", "in1 300 IN A 10.0.0.1\n")
    main = _write(tmp_path, "main.zone", HEADER + "$INCLUDE inc.zone\n")
    resolver = dns.zonefile.IncludeResolver()
    _load(main, include_resolver=resolver)
    assert resolver.depth == 0
