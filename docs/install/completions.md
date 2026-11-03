# Shell completions

The `aplexica completion` command generates completions for Bash, Zsh, Fish,
and PowerShell. Install the generated file in the location used by your shell,
then start a new shell (or reload its completion configuration).

## Bash

```bash
mkdir -p "${XDG_DATA_HOME:-$HOME/.local/share}/bash-completion/completions"
aplexica completion bash > "${XDG_DATA_HOME:-$HOME/.local/share}/bash-completion/completions/aplexica"
```

The standard Bash completion directories are searched when the completion
helper is installed. On systems that use `/etc/bash_completion.d`, write the
file there instead if that directory is already part of your Bash setup.

## Zsh

```zsh
mkdir -p "${XDG_DATA_HOME:-$HOME/.local/share}/zsh/site-functions"
aplexica completion zsh > "${XDG_DATA_HOME:-$HOME/.local/share}/zsh/site-functions/_aplexica"
```

Make sure the directory is included in `fpath` before `compinit` runs. For
example, add the following to `.zshrc` if needed:

```zsh
fpath=("${XDG_DATA_HOME:-$HOME/.local/share}/zsh/site-functions" $fpath)
autoload -Uz compinit && compinit
```

## Fish

```fish
mkdir -p ~/.config/fish/completions
aplexica completion fish > ~/.config/fish/completions/aplexica.fish
```

Fish loads completion files from `~/.config/fish/completions` automatically.

## PowerShell

```powershell
$dir = Split-Path -Parent $PROFILE
New-Item -ItemType Directory -Force -Path $dir | Out-Null
aplexica completion powershell | Set-Content -Path (Join-Path $dir 'aplexica.ps1') -Encoding utf8
```

`$PROFILE` names the profile file PowerShell loads for you, so this writes the
completion script next to it. Load it from that profile:

```powershell
. (Join-Path (Split-Path -Parent $PROFILE) 'aplexica.ps1')
```

`-Encoding utf8` is deliberate: `utf8NoBOM` exists only in PowerShell 6 and
later, and Windows PowerShell 5.1, which is what Windows 10 and 11 ship by
default, rejects it. Windows PowerShell writes this file with a byte-order
mark and PowerShell 7 writes it without one; both load it correctly.
