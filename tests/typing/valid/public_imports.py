# The documented public imports must not be flagged by type checkers as private
# (implicit) re-exports.
from pyinfra import __version__, config, host, inventory, state
from pyinfra.api import (
    BaseStateCallback,
    Config,
    ConfirmedStep,
    DeployError,
    FactBase,
    FactError,
    FactNotCollected,
    FactPreconditionError,
    FactProcessError,
    FactTypeError,
    FactValueError,
    FileDownloadCommand,
    FileUploadCommand,
    FunctionCommand,
    HiddenValue,
    Host,
    HostIdentity,
    HostPlanRecord,
    Inventory,
    InventoryError,
    MaskString,
    MissingCommandError,
    OperationError,
    OperationTypeError,
    OperationValueError,
    PlanReuseSnapshot,
    PlanReuseStore,
    QuoteString,
    RsyncCommand,
    ShortFactBase,
    State,
    StringCommand,
    deploy,
    operation,
    resolve_host_identity,
)
from pyinfra.connectors.scp import SCPClient
from pyinfra.connectors.sshuserclient import SSHClient

print(__version__, config, host, inventory, state)
print(BaseStateCallback, Config, DeployError, FactBase, FactError, FactNotCollected)
print(FactPreconditionError, FactProcessError, FactTypeError, FactValueError)
print(FileDownloadCommand, FileUploadCommand, FunctionCommand, HiddenValue, Host)
print(Inventory, InventoryError, MaskString, MissingCommandError, OperationError)
print(OperationTypeError, OperationValueError, QuoteString, RsyncCommand)
print(ShortFactBase, State, StringCommand, deploy, operation, SCPClient, SSHClient)
print(
    HostIdentity,
    HostPlanRecord,
    PlanReuseSnapshot,
    PlanReuseStore,
    ConfirmedStep,
    resolve_host_identity,
)
