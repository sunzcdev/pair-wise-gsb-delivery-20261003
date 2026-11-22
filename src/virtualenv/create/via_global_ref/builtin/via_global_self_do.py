from __future__ import annotations

__lazy_modules__ = [
    "virtualenv.util.path",
    f"{__spec__.parent}.ref",  # ty: ignore[missing-union-attribute]  # imported modules have a spec
]

from abc import ABC
from typing import TYPE_CHECKING

from virtualenv.create.plan import Action, ActionKind, ActionMethod, ActionPlan, Fallback
from virtualenv.create.via_global_ref.api import ViaGlobalRefApi, ViaGlobalRefMeta
from virtualenv.util.path import ensure_within

from .builtin_way import VirtualenvBuiltin
from .ref import (
    ExePathRef,
    ExePathRefToDest,
    RefMust,
    RefWhen,
    applies,
)

if TYPE_CHECKING:
    from collections.abc import Generator
    from pathlib import Path

    from python_discovery import PythonInfo

    from virtualenv.config.cli.parser import VirtualEnvOptions
    from virtualenv.create.via_global_ref.venv import Venv

    from .ref import PathRef


class ViaGlobalRefVirtualenvBuiltin(ViaGlobalRefApi, VirtualenvBuiltin, ABC):
    """Built-in creator that links or copies the executables and libraries of a global interpreter.

    :param options: the parsed command line options
    :param interpreter: the interpreter the environment is created from

    :var enable_system_site_package: ``True`` to give the environment access to the system site-packages

    """

    def __init__(self, options: VirtualEnvOptions, interpreter: PythonInfo) -> None:
        super().__init__(options, interpreter)
        self._sources: list[PathRef] = (
            getattr(options.meta, "sources", None) or []
        )  # if created as a describer this might be missing

    @classmethod
    def can_create(cls, interpreter: PythonInfo) -> BuiltinViaGlobalRefMeta | None:
        """By default, all built-in methods assume that if we can describe it we can create it.

        :param interpreter: the host interpreter to create the environment from

        :returns: the metadata describing how to create the environment, ``None`` if it cannot be created

        """
        # first we must be able to describe it
        if not cls.can_describe(interpreter):
            return None
        meta = cls.setup_meta(interpreter)
        if meta is not None and meta:
            cls._sources_can_be_applied(interpreter, meta)
        return meta

    @classmethod
    # the base hook signature, this meta does not depend on the interpreter
    def setup_meta(cls, interpreter: PythonInfo) -> BuiltinViaGlobalRefMeta:  # ruff:ignore[unused-class-method-argument]
        """Return the metadata for a built-in creator, which does not depend on the interpreter.

        :param interpreter: the host interpreter, unused

        :returns: a new metadata object

        """
        return BuiltinViaGlobalRefMeta()

    @classmethod
    def _sources_can_be_applied(cls, interpreter: PythonInfo, meta: BuiltinViaGlobalRefMeta) -> None:
        for src in cls.sources(interpreter):
            if src.exists:
                cls._record_present_source(meta, src)
            else:
                cls._record_missing_source(meta, src)
            if not meta.can_copy and not meta.can_symlink:
                meta.error = f"neither copy or symlink supported, copy: {meta.copy_error} symlink: {meta.symlink_error}"
            if meta.error:
                break
            meta.sources.append(src)

    @classmethod
    def sources(cls, interpreter: PythonInfo) -> Generator[ExePathRefToDest]:
        """Yield the references to the host executables this creator links or copies.

        :param interpreter: the host interpreter to take the executables from

        :yields: the executable references, each pointing into the environment's bin folder

        """
        for host_exe, targets, must, when in cls._executables(interpreter):
            yield ExePathRefToDest(host_exe, dest=cls.to_bin, targets=targets, must=must, when=when)

    @classmethod
    def _executables(cls, interpreter: PythonInfo) -> Generator[tuple[Path, list[str], str, str]]:
        raise NotImplementedError

    def to_bin(self, src: Path) -> Path:
        """Map a host file to its location in the environment's bin folder.

        :param src: the host file

        :returns: the path of the file inside the bin folder

        """
        return self.bin_dir / src.name

    def action_plan(self) -> ActionPlan:
        """Describe the files the creator would copy or link, without writing anything.

        The actions come from the same ``sources`` the creation applies, so the view and the real environment cannot
        disagree about which files are used or where they land.

        :returns: the read-only view of the creation

        """
        return ActionPlan(
            creator=type(self).__name__,
            symlinks=self.symlinks,
            mode_reason=self.symlink_reason,
            actions=[self._action_for(ref) for ref in self._plan_sources() if applies(ref.when, self.symlinks)],
        )

    def _plan_sources(self) -> list[PathRef]:
        # the plan reads the very list creation iterates, so the two cannot disagree about the files in play
        return list(self._sources)

    def _action_for(self, ref: PathRef) -> Action:
        chosen = ActionMethod.SYMLINK if ref.method_kind(self.symlinks) == RefMust.SYMLINK else ActionMethod.COPY
        destinations = ref.destinations(self)
        return Action(
            src=ref.src,
            dest=destinations[0] if destinations else None,
            kind=self._action_kind(ref),
            method=chosen,
            reason=self._method_reason(ref, chosen),
            aliases=destinations[1:],
            exists=ref.exists,
            fallback=self._fallback_for(ref, chosen),
        )

    @staticmethod
    def _action_kind(ref: PathRef) -> str:
        if isinstance(ref, ExePathRef):
            return ActionKind.EXECUTABLE
        if ref.exists and ref.src.is_dir():
            return ActionKind.DIRECTORY
        return ActionKind.FILE

    @staticmethod
    def _method_reason(ref: PathRef, chosen: str) -> str:
        if ref.must == RefMust.COPY:
            return "the creator always copies this file, linking it is unreliable"
        if ref.must == RefMust.SYMLINK:
            return "the creator always links this file, the environment needs it at the host location"
        if chosen == ActionMethod.SYMLINK:
            return "symlinks are enabled for the environment"
        return "copy is used when symlinks are not enabled"

    def _fallback_for(self, ref: PathRef, chosen: str) -> Fallback | None:
        available, reason = self._method_capability(ref, chosen)
        if available:
            return None
        alternative = ActionMethod.COPY if chosen == ActionMethod.SYMLINK else ActionMethod.SYMLINK
        alt_available, _ = self._method_capability(ref, alternative)
        return Fallback(alternative if alt_available else None, reason, alt_available)

    def _method_capability(self, ref: PathRef, method: str) -> tuple[bool, str]:
        meta = self._meta
        if method == ActionMethod.SYMLINK:
            if meta is not None and meta.symlink_error:
                return False, meta.symlink_error
            if ref.can_symlink:
                return True, ""
            return False, self._symlink_blocker(ref)
        if meta is not None and meta.copy_error:
            return False, meta.copy_error
        if ref.can_copy:
            return True, ""
        if not ref.exists:
            return False, "the host file is missing"
        if not ref.can_read:
            return False, "the host file cannot be read"
        return False, "the host file cannot be copied"

    @staticmethod
    def _symlink_blocker(ref: PathRef) -> str:
        if not ref.FS_SUPPORTS_SYMLINK:
            return "the filesystem does not support symlinks"
        if not ref.can_read:
            return "the host file cannot be read"
        return "the host executable is not marked executable"

    @staticmethod
    def _record_present_source(meta: BuiltinViaGlobalRefMeta, src: ExePathRefToDest) -> None:
        if meta.can_copy and not src.can_copy:
            meta.copy_error = f"cannot copy {src}"
        if meta.can_symlink and not src.can_symlink:
            meta.symlink_error = f"cannot symlink {src}"

    @staticmethod
    def _record_missing_source(meta: BuiltinViaGlobalRefMeta, src: ExePathRefToDest) -> None:
        msg = f"missing required file {src}"
        if src.when == RefWhen.ANY:
            meta.error = msg
        elif src.when == RefWhen.COPY:
            meta.copy_error = msg
        elif src.when == RefWhen.SYMLINK:
            meta.symlink_error = msg

    def create(self) -> None:
        """Create the directories, the configuration and the linked or copied files of the environment.

        :raises ValueError: when a directory lies outside the destination folder

        """
        dirs = self.ensure_directories()
        for directory in list(dirs):
            if any(i for i in dirs if i is not directory and directory.parts == i.parts[: len(directory.parts)]):
                dirs.remove(directory)
        for directory in sorted(dirs):
            ensure_within(self.dest, directory)

        self.set_pyenv_cfg()
        self.pyenv_cfg.write()
        true_system_site = self.enable_system_site_package
        try:
            self.enable_system_site_package = False
            for src in self._sources:
                if applies(src.when, self.symlinks):
                    src.run(self, self.symlinks)
        finally:
            if true_system_site != self.enable_system_site_package:
                self.enable_system_site_package = true_system_site
        super().create()

    def ensure_directories(self) -> set[Path]:
        """Return the directories the environment needs.

        :returns: the destination, bin, script, stdlib, include and library directories

        """
        return {self.dest, self.bin_dir, self.script_dir, self.stdlib, self.include_dir} | set(self.libs)

    @property
    def include_dir(self) -> Path:
        """The include directory of the environment, named ``Include`` on Windows."""
        return self.dest / ("Include" if self.interpreter.os == "nt" else "include")

    def set_pyenv_cfg(self) -> None:
        """We inject the base prefix and base exec prefix directly, so site.py need not discover them from home.

        The interpreter itself usually discovers them from home, which is what the injection avoids.

        """
        super().set_pyenv_cfg()
        self.pyenv_cfg["base-prefix"] = self.interpreter.system_prefix
        self.pyenv_cfg["base-exec-prefix"] = self.interpreter.system_exec_prefix
        self.pyenv_cfg["base-executable"] = self.interpreter.system_exe

    def install_venv_shared_libs(self, venv_creator: Venv) -> None:
        """Install shared libraries next to the environment, a no-op for this creator.

        :param venv_creator: the ``venv`` creator that built the environment

        """


class BuiltinViaGlobalRefMeta(ViaGlobalRefMeta):
    """Capability metadata that also lists the files the environment links to or copies.

    :var sources: the references to the host files, in the order they are applied

    """

    def __init__(self) -> None:
        super().__init__()
        self.sources: list[PathRef] = []


__all__ = [
    "BuiltinViaGlobalRefMeta",
    "ViaGlobalRefVirtualenvBuiltin",
]
