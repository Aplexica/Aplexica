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
New-Item -ItemType Directory -Force -Path $HOME\.config\powershell | Out-Null
aplexica completion powershell | Out-File -Encoding utf8NoBOM $HOME\.config\powershell\aplexica.ps1
```

Load the file from your PowerShell profile:

```powershell
. $HOME\.config\powershell\aplexica.ps1
```

Use `$PROFILE` to find the profile file that PowerShell loads for your user.
