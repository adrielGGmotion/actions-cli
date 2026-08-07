---
name: remote-build
description: Offload resource-heavy builds, compilation, tests, containers, and similar commands from a low-spec local machine through the `remote` CLI while keeping local files authoritative. Use when a project has `.remote.yml`, `remote` is installed, and a command is likely to consume substantial CPU, memory, storage, or time.
---

# Remote builds

Keep all editing, source inspection, and lightweight operations local. Use `remote` only for resource-heavy commands.

## Run a command

1. Confirm `remote` is available and the repository contains `.remote.yml`.
2. Run from the directory where the original command would run.
3. Preserve argument boundaries by passing the executable and arguments directly:

```console
remote ./gradlew assembleDebug
remote cargo build --release
remote npm run build
```

Do not wrap the command in `sh -c` unless the command genuinely requires shell syntax. Do not create temporary commits, branches, workflows, remote IDE sessions, or remote development environments.

Treat the local working tree as the only authoritative workspace. After completion, inspect the restored configured outputs locally. Diagnose failures from the returned stdout, stderr, and exit status; make fixes locally and rerun only when justified.

## Safety

- Never place GitHub tokens, encryption identities, or secrets in `.remote.yml`, source files, command arguments, or chat.
- Remember that ignored files are excluded by default. Do not force ignored secret files into a job.
- Check `.remote.yml` output patterns before relying on a generated file being restored.
- Avoid interactive commands; remote execution has no interactive terminal.
- Do not assume project-specific credentials exist on the runner.
- Use ordinary local commands when the operation is inexpensive or modifies the working tree directly.
