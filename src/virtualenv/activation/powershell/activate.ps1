<#
.Synopsis
Activate a Python virtual environment for the current PowerShell session.

.Description
Pushes the python executable for a virtual environment to the front of the
$Env:PATH environment variable and sets the prompt to signify that you are
in a Python virtual environment. Makes use of the command line switches as
well as the `pyvenv.cfg` file values present in the virtual environment.

By default, activating while another environment is already active replaces
it: the previous environment is torn down first, so a single `deactivate`
returns the session to how it was before any activation. Pass -Nested to
stack the new environment on top of the active one instead. Every nested
layer owns its own PATH prefix, VIRTUAL_ENV/VIRTUAL_ENV_PROMPT values, prompt
function and pydoc function, and each `deactivate` removes one layer until
the stack is empty, at which point the session is fully restored and the
`deactivate` function removes itself.

.Parameter VenvDir
Path to the directory that contains the virtual environment to activate. The
default value for this is the parent of the directory that the activate.ps1
script is located within.

.Parameter Prompt
The prompt prefix to display when this virtual environment is activated. By
default, this prompt is the name of the virtual environment folder (VenvDir)
surrounded by parentheses and followed by a single space (ie. '(.venv) ').

.Parameter Nested
Keep an already active virtual environment on a stack instead of replacing it.
The new environment becomes the active layer; each later `deactivate` removes
one layer and restores the layer below it, so the first environment stays
usable again after the second is deactivated. Activating the environment that
is already the active layer is a no-op. Without this switch, activation first
deactivates every active layer, matching the non-nesting behavior.

.Example
activate.ps1
Activates the Python virtual environment that contains the activate.ps1 script.

.Example
activate.ps1 -Verbose
Activates the Python virtual environment that contains the activate.ps1 script,
and shows extra information about the activation as it executes.

.Example
activate.ps1 -VenvDir C:\Users\MyUser\Common\.venv
Activates the Python virtual environment located in the specified location.

.Example
activate.ps1 -Prompt "MyPython"
Activates the Python virtual environment that contains the activate.ps1 script,
and prefixes the current prompt with the specified string (surrounded in
parentheses) while the virtual environment is active.

.Example
activate.ps1 -Nested
Stacks this virtual environment on top of the one already active in the
session. Run `deactivate` once per layer to return through the environments in
reverse order; the PATH, environment names and prompt of every layer are
restored as its layer is removed.

.Notes
On Windows, it may be required to enable this activate.ps1 script by setting the
execution policy for the user. You can do this by issuing the following PowerShell
command:

PS C:\> Set-ExecutionPolicy -ExecutionPolicy RemoteSigned -Scope CurrentUser

For more information on Execution Policies:
https://go.microsoft.com/fwlink/?LinkID=135170

#>
Param(
    [Parameter(Mandatory = $false)]
    [String]
    $VenvDir,
    [Parameter(Mandatory = $false)]
    [String]
    $Prompt,
    [Parameter(Mandatory = $false)]
    [Switch]
    $Nested
)

function Get-PyVenvConfig(
    [String]
    $ConfigDir
) {
    Write-Verbose "Given ConfigDir=$ConfigDir, obtain values in pyvenv.cfg"

    $pyvenvConfigPath = [IO.Path]::Combine((Get-Location -PSProvider FileSystem).ProviderPath, $ConfigDir, 'pyvenv.cfg')

    $pyvenvConfig = @{ }

    if ([IO.File]::Exists($pyvenvConfigPath)) {
        Write-Verbose "File exists, parse ``key = value`` lines"

        [IO.File]::ReadAllLines($pyvenvConfigPath) | ForEach-Object {
            $keyval = $PSItem -split "\s*=\s*", 2
            if ($keyval[0] -and $keyval[1]) {
                $val = $keyval[1]

                if ("'""".Contains($val.Substring(0, 1))) {
                    $val = $val.Substring(1, $val.Length - 2)
                }

                $pyvenvConfig[$keyval[0]] = $val
                Write-Verbose "Adding Key: '$($keyval[0])'='$val'"
            }
        }
    } else {
        Write-Error "Cannot find configuration file '$pyvenvConfigPath'" -ErrorAction Continue
    }
    return $pyvenvConfig
}

function global:New-VirtualEnvPrompt {
    # Build one layer's prompt wrapper with the prefix literal and the previous prompt script block baked into a fresh
    # script block. Closures capture variables by name and a nested call would rebind the same names, so values are
    # embedded here instead: every layer keeps its own prefix and a direct handle to the prompt it was stacked over.
    # The returned handle names a global variable holding the previous prompt; the caller deletes that variable when
    # this layer is deactivated, so no wrapper's handle outlives its layer.
    param(
        [String] $Prefix,
        [scriptblock] $Previous
    )
    $safePrefix = $Prefix.Replace("'", "''")
    $handle = [guid]::NewGuid().ToString("N")
    Set-Variable -Scope global -Name "_VIRTUAL_ENV_PROMPT_PREV_$handle" -Value $Previous
    [pscustomobject]@{
        Handle = $handle
        Prompt = [scriptblock]::Create(
            "Write-Host -NoNewline -ForegroundColor Green '($safePrefix) '; " +
            "& `$global:_VIRTUAL_ENV_PROMPT_PREV_$handle"
        )
    }
}

function global:Remove-VirtualEnvPromptHandle {
    param([AllowNull()] [String] $Handle)
    if ($Handle) {
        Remove-Variable -Scope global -Name "_VIRTUAL_ENV_PROMPT_PREV_$Handle" -ErrorAction SilentlyContinue
    }
}

function global:deactivate([switch] $NonDestructive) {
    # The bottom active layer lives in the flat _OLD_VIRTUAL_* variables; every layer it suspended is one entry of the
    # global $_VIRTUAL_ENV_STACK below it. Popping the top frame restores the layer below, an empty stack ends the
    # activation. The $global: scope is mandatory: an unqualified name would resolve to a function-local variable here.
    $frame = if ((Test-Path variable:global:_VIRTUAL_ENV_STACK) -and $global:_VIRTUAL_ENV_STACK.Count -gt 0) {
        $global:_VIRTUAL_ENV_STACK[$global:_VIRTUAL_ENV_STACK.Count - 1]
    } else {
        $null
    }
    if ($null -ne $frame) {
        $global:_VIRTUAL_ENV_STACK.RemoveAt($global:_VIRTUAL_ENV_STACK.Count - 1)
    }

    # The leaving layer's wrapper always owns the flat handle variable, regardless of whether a suspended frame exists
    # below it. Read it before any frame restore overwrites the variable; frames keep their own handles for later.
    $leavingHandle = if (Test-Path variable:_VIRTUAL_ENV_PROMPT_HANDLE) {
        $variable:_VIRTUAL_ENV_PROMPT_HANDLE
    } else {
        $null
    }

    if (Test-Path variable:_OLD_VIRTUAL_PATH) {
        $env:PATH = $variable:_OLD_VIRTUAL_PATH
        Remove-Variable "_OLD_VIRTUAL_PATH" -Scope global
    }

    if (Test-Path variable:_OLD_VIRTUAL_TCL_LIBRARY) {
        $env:TCL_LIBRARY = $variable:_OLD_VIRTUAL_TCL_LIBRARY
        Remove-Variable "_OLD_VIRTUAL_TCL_LIBRARY" -Scope global
    }

    if (Test-Path variable:_OLD_VIRTUAL_TK_LIBRARY) {
        $env:TK_LIBRARY = $variable:_OLD_VIRTUAL_TK_LIBRARY
        Remove-Variable "_OLD_VIRTUAL_TK_LIBRARY" -Scope global
    }

    if (Test-Path variable:_OLD_PKG_CONFIG_PATH) {
        $env:PKG_CONFIG_PATH = $variable:_OLD_PKG_CONFIG_PATH
        Remove-Variable "_OLD_PKG_CONFIG_PATH" -Scope global
    }

    if (Test-Path variable:_OLD_VIRTUAL_PYTHONHOME) {
        if ($null -eq $variable:_OLD_VIRTUAL_PYTHONHOME) {
            Remove-Item env:PYTHONHOME -ErrorAction SilentlyContinue
        } else {
            $env:PYTHONHOME = $variable:_OLD_VIRTUAL_PYTHONHOME
        }
        Remove-Variable "_OLD_VIRTUAL_PYTHONHOME" -Scope global
    }

    if (Test-Path function:_old_virtual_prompt) {
        $function:prompt = $function:_old_virtual_prompt
        Remove-Item function:\_old_virtual_prompt
    }

    if (Test-Path variable:_OLD_VIRTUAL_PYDOC) {
        if ($null -eq $variable:_OLD_VIRTUAL_PYDOC) {
            Remove-Item function:pydoc -ErrorAction SilentlyContinue
        } else {
            $function:pydoc = $variable:_OLD_VIRTUAL_PYDOC
        }
        Remove-Variable "_OLD_VIRTUAL_PYDOC" -Scope global
    }

    if (Get-Variable -Name "_PYTHON_VENV_PROMPT_PREFIX" -ErrorAction SilentlyContinue) {
        Remove-Variable -Name _PYTHON_VENV_PROMPT_PREFIX -Scope Global -Force
    }

    if ($null -ne $frame) {
        # Recreate the suspended layer's flat state so a later deactivate removes that layer too. Each layer is only
        # responsible for its own PATH prefix, environment names and prompt; the saved values hand control back to it.
        $env:VIRTUAL_ENV = $frame.VirtualEnv
        $env:VIRTUAL_ENV_PROMPT = $frame.VirtualEnvPrompt
        New-Variable -Scope global -Name _OLD_VIRTUAL_PATH -Value $frame.OldPath
        New-Variable -Scope global -Name _OLD_PKG_CONFIG_PATH -Value $frame.OldPkgConfig
        New-Variable -Scope global -Name _OLD_VIRTUAL_PYTHONHOME -Value $frame.OldPythonHome
        New-Variable -Scope global -Name _OLD_VIRTUAL_PYDOC -Value $frame.OldPydoc
        if ($frame.Contains("OldTclLibrary")) {
            New-Variable -Scope global -Name _OLD_VIRTUAL_TCL_LIBRARY -Value $frame.OldTclLibrary
        }
        if ($frame.Contains("OldTkLibrary")) {
            New-Variable -Scope global -Name _OLD_VIRTUAL_TK_LIBRARY -Value $frame.OldTkLibrary
        }
        if ($null -ne $frame.OldPrompt) {
            # Restore the suspended layer's wrapper verbatim. It already prints that layer's prefix and calls its own
            # parent prompt through the handle stored in the frame, so no new wrapper (and no extra handle) is created.
            New-Variable -Name _PYTHON_VENV_PROMPT_PREFIX `
                -Description "Python virtual environment prompt prefix" `
                -Scope Global -Option ReadOnly -Visibility Public -Value $frame.PromptPrefix
            $function:prompt = [scriptblock]::Create($frame.OldPrompt.ToString())
            if ($frame.Contains("PromptHandle")) {
                Set-Variable -Scope global -Name _VIRTUAL_ENV_PROMPT_HANDLE -Value $frame.PromptHandle
            }
            function global:_old_virtual_prompt {
                ""
            }
            # Recreate the inner helper name from the wrapper's backing variable so a later nested activation can
            # snapshot the layer consistently again.
            if ($frame.Contains("PromptHandle")) {
                $backingName = "_VIRTUAL_ENV_PROMPT_PREV_$($frame.PromptHandle)"
                $function:_old_virtual_prompt = Get-Variable -Scope global -Name $backingName -ValueOnly
            }
        }
    } else {
        if ($env:VIRTUAL_ENV) {
            Remove-Item env:VIRTUAL_ENV -ErrorAction SilentlyContinue
        }

        if ($env:VIRTUAL_ENV_PROMPT) {
            Remove-Item env:VIRTUAL_ENV_PROMPT -ErrorAction SilentlyContinue
        }
    }

    # Delete only the wrapper handle the layer that is leaving owned, captured before the frame rebuild ran.
    Remove-VirtualEnvPromptHandle -Handle $leavingHandle
    if ($null -eq $frame) {
        Remove-Variable "_VIRTUAL_ENV_PROMPT_HANDLE" -Scope global -ErrorAction SilentlyContinue
    }

    # The stack holds suspended layers while the flat _OLD_VIRTUAL_* variables hold the active (bottom) layer. With no
    # frame and no flat state left, every layer is gone, so the command removes itself; otherwise it stays to pop again.
    if (!$NonDestructive -and $null -eq $frame -and !(Test-Path variable:_OLD_VIRTUAL_PATH)) {
        Remove-Variable "_VIRTUAL_ENV_STACK" -Scope global -ErrorAction SilentlyContinue
        Remove-Item function:deactivate
    }
}

$VenvExecPath = [IO.Path]::GetDirectoryName($MyInvocation.MyCommand.Definition)

Write-Verbose "Activation script is located in path: '$VenvExecPath'"

if ($VenvDir) {
    try {
        $VenvDir = $VenvDir -replace '^(?:Microsoft.PowerShell.Core\\)?FileSystem::', ''
        if ($VenvDir -eq '~') {
            $VenvDir = $HOME
        } elseif ($VenvDir -match '^~[/\\]') {
            $VenvDir = [IO.Path]::Combine($HOME, $VenvDir.Substring(2))
        }
        if ([IO.Path]::DirectorySeparatorChar -eq '/' -and $VenvDir -match '^([^/:]+):(.*)$') {
            $VenvTail = $Matches[2]
            $VenvDrive = Get-PSDrive -Name $Matches[1] -PSProvider FileSystem -ErrorAction Stop
            if ($VenvTail.StartsWith('/')) {
                $VenvDir = [IO.Path]::Combine($VenvDrive.Root, $VenvTail.TrimStart('/'))
            } else {
                $VenvDir = [IO.Path]::Combine($VenvDrive.Root, $VenvDrive.CurrentLocation, $VenvTail)
            }
        } elseif ($VenvDir -match '^([^/\\:]+):') {
            Get-PSDrive -Name $Matches[1] -PSProvider FileSystem -ErrorAction Stop | Out-Null
            $VenvDir = $ExecutionContext.SessionState.Path.GetUnresolvedProviderPathFromPSPath($VenvDir)
        }
        $VenvDir = [IO.Path]::GetFullPath([IO.Path]::Combine((Get-Location -PSProvider FileSystem).ProviderPath, $VenvDir))
    } catch {
        throw "Cannot resolve environment directory '$VenvDir': $_"
    }
    Write-Verbose "VenvDir given as parameter, using '$VenvDir' to determine values"
} else {
    $VenvDir = [IO.Path]::GetDirectoryName($VenvExecPath)
    Write-Verbose "VenvDir=$VenvDir"
}

$pyvenvCfg = Get-PyVenvConfig -ConfigDir $VenvDir

if ($Prompt) {
    Write-Verbose "Prompt specified as argument, using '$Prompt'"
} else {
    if ($pyvenvCfg -and $pyvenvCfg['prompt']) {
        Write-Verbose "Setting based on value in pyvenv.cfg='$($pyvenvCfg['prompt'])'"
        $Prompt = $pyvenvCfg['prompt']
    } elseif (__VIRTUAL_PROMPT__ -ne "") {
        $Prompt = __VIRTUAL_PROMPT__
    } else {
        $Prompt = [IO.Path]::GetFileName(
            $VenvDir.TrimEnd([IO.Path]::DirectorySeparatorChar, [IO.Path]::AltDirectorySeparatorChar)
        )
    }
}

if (-not (Test-Path variable:_VIRTUAL_ENV_STACK)) {
    New-Variable -Scope global -Name _VIRTUAL_ENV_STACK -Value (New-Object 'System.Collections.Generic.List[hashtable]')
}

if ($Nested -and $env:VIRTUAL_ENV) {
    if ($env:VIRTUAL_ENV -eq $VenvDir) {
        # Re-activating the layer already on top changes nothing: the same PATH prefix, environment names and prompt
        # stay active, so do not push a duplicate frame that a single deactivate would have to unwind.
        Write-Verbose "Virtual environment '$VenvDir' is already the active layer, leaving the stack untouched"
        return
    }

    Write-Verbose "Nesting '$VenvDir' on top of active virtual environment '$env:VIRTUAL_ENV'"

    # Snapshot the active layer before the standard activation body overwrites its flat variables, then clear those
    # variables so the body's New-Variable calls do not collide. A deactivate of the new layer restores this frame.
    $frame = [ordered]@{
        VirtualEnv = $env:VIRTUAL_ENV
        VirtualEnvPrompt = $env:VIRTUAL_ENV_PROMPT
        OldPath = $variable:_OLD_VIRTUAL_PATH
        OldPkgConfig = $variable:_OLD_PKG_CONFIG_PATH
        OldPythonHome = $variable:_OLD_VIRTUAL_PYTHONHOME
        OldPydoc = $variable:_OLD_VIRTUAL_PYDOC
    }
    if (Test-Path variable:_OLD_VIRTUAL_TCL_LIBRARY) {
        $frame["OldTclLibrary"] = $variable:_OLD_VIRTUAL_TCL_LIBRARY
    }
    if (Test-Path variable:_OLD_VIRTUAL_TK_LIBRARY) {
        $frame["OldTkLibrary"] = $variable:_OLD_VIRTUAL_TK_LIBRARY
    }
    if (Test-Path function:_old_virtual_prompt) {
        # Save the parent layer's complete wrapper (with its own prefix and parent prompt) plus the handle of the global
        # variable that wrapper calls. Deactivation restores this wrapper and deletes that same handle, so each layer's
        # wrapper and its backing variable are created and destroyed together instead of leaking a rebuilt handle.
        $frame["OldPrompt"] = [scriptblock]::Create($function:prompt.ToString())
        $frame["PromptPrefix"] = $variable:_PYTHON_VENV_PROMPT_PREFIX
        if (Test-Path variable:_VIRTUAL_ENV_PROMPT_HANDLE) {
            $frame["PromptHandle"] = $variable:_VIRTUAL_ENV_PROMPT_HANDLE
        }
    }
    $_VIRTUAL_ENV_STACK.Add([hashtable]$frame)

    Remove-Variable "_OLD_VIRTUAL_PATH" -Scope global -ErrorAction SilentlyContinue
    Remove-Variable "_OLD_PKG_CONFIG_PATH" -Scope global -ErrorAction SilentlyContinue
    Remove-Variable "_OLD_VIRTUAL_PYTHONHOME" -Scope global -ErrorAction SilentlyContinue
    Remove-Variable "_OLD_VIRTUAL_PYDOC" -Scope global -ErrorAction SilentlyContinue
    Remove-Variable "_OLD_VIRTUAL_TCL_LIBRARY" -Scope global -ErrorAction SilentlyContinue
    Remove-Variable "_OLD_VIRTUAL_TK_LIBRARY" -Scope global -ErrorAction SilentlyContinue
    # The parent's handle now lives in its frame; clear the flat slot so the child's activation can claim it.
    Remove-Variable "_VIRTUAL_ENV_PROMPT_HANDLE" -Scope global -ErrorAction SilentlyContinue
    # Drop the ReadOnly prefix so the new layer can declare its own; the frame above kept the parent's value. The prompt
    # functions stay in place on purpose: if the new layer shows a prompt it overwrites both just below, and if it hides
    # the prompt, keeping the parent's function means restoring the frame still yields a working prompt instead of an
    # _old_virtual_prompt call into a removed function.
    if (Get-Variable -Name "_PYTHON_VENV_PROMPT_PREFIX" -ErrorAction SilentlyContinue) {
        Remove-Variable -Name _PYTHON_VENV_PROMPT_PREFIX -Scope Global -Force
    }
} else {
    # Default, non-nesting behavior: tear down every active layer first so a single deactivate restores the session as
    # it was before any activation. Each call pops one frame; -NonDestructive keeps the function itself, so loop on the
    # remaining state rather than on the function's existence.
    while ($env:VIRTUAL_ENV -or (Test-Path variable:_OLD_VIRTUAL_PATH)) {
        deactivate -nondestructive
    }
}

New-Variable -Scope global -Name _OLD_VIRTUAL_PYDOC -Value $function:pydoc
function global:pydoc {
    & python -m pydoc $args
}

$env:VIRTUAL_ENV = $VenvDir
$env:VIRTUAL_ENV_PROMPT = $Prompt

New-Variable -Scope global -Name _OLD_VIRTUAL_PYTHONHOME -Value $env:PYTHONHOME
Remove-Item env:PYTHONHOME -ErrorAction SilentlyContinue

# a $null saved value tells deactivate to remove the variable
if (__TCL_LIBRARY__ -ne "") {
    New-Variable -Scope global -Name _OLD_VIRTUAL_TCL_LIBRARY -Value $env:TCL_LIBRARY
    $env:TCL_LIBRARY = __TCL_LIBRARY__
}

if (__TK_LIBRARY__ -ne "") {
    New-Variable -Scope global -Name _OLD_VIRTUAL_TK_LIBRARY -Value $env:TK_LIBRARY
    $env:TK_LIBRARY = __TK_LIBRARY__
}

New-Variable -Scope global -Name _OLD_VIRTUAL_PATH -Value $env:PATH

New-Variable -Scope global -Name _OLD_PKG_CONFIG_PATH -Value $env:PKG_CONFIG_PATH
$env:PKG_CONFIG_PATH = [IO.Path]::Combine($env:VIRTUAL_ENV, "lib", "pkgconfig")
if ($_OLD_PKG_CONFIG_PATH) {
    $env:PKG_CONFIG_PATH += __PATH_SEP__ + $_OLD_PKG_CONFIG_PATH
}

$env:PATH = "$env:VIRTUAL_ENV/" + __BIN_NAME__
if ($_OLD_VIRTUAL_PATH) {
    $env:PATH += __PATH_SEP__ + $_OLD_VIRTUAL_PATH
}

if (!$env:VIRTUAL_ENV_DISABLE_PROMPT) {
    # Snapshot the current prompt as a detached script block before anything overwrites the prompt function. Reading
    # $function:prompt after that assignment would follow the function reference back into the new wrapper and recurse.
    $previousPrompt = [scriptblock]::Create($function:prompt.ToString())
    function global:_old_virtual_prompt {
        ""
    }
    $function:_old_virtual_prompt = $previousPrompt
    New-Variable -Name _PYTHON_VENV_PROMPT_PREFIX -Description "Python virtual environment prompt prefix" -Scope Global -Option ReadOnly -Visibility Public -Value $Prompt

    # The factory gives this layer a wrapper whose handle is recorded here. A nested activation moves that handle into
    # the layer's frame, and deactivate removes exactly the owning layer's handle, so nothing leaks into the session.
    $builtPrompt = New-VirtualEnvPrompt -Prefix $Prompt -Previous $previousPrompt
    $function:prompt = $builtPrompt.Prompt
    New-Variable -Scope global -Name _VIRTUAL_ENV_PROMPT_HANDLE -Value $builtPrompt.Handle
}
