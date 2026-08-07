# remote

`remote` is a small FOSS CLI that sends a snapshot of the current Git working tree to an ephemeral GitHub Actions runner, executes one command, and restores configured outputs locally. The local filesystem remains authoritative: no project commits, branches, index changes, or project workflows are created.

```console
remote ./gradlew assembleDebug
remote cargo build --release
remote npm run build
```

## Security design

The worker repository is a transport and execution service, not the project repository. Each job uses an opaque prerelease with two assets:

1. `request.age` contains the workspace, exact argv, relative working directory, output patterns, cache declaration, and an ephemeral reply public key. It is encrypted to the worker's age X25519 recipient.
2. `result.age` contains stdout, stderr, exit status, and configured regular-file outputs. It is encrypted to the per-job ephemeral reply recipient.

Only the random job identifier crosses `workflow_dispatch`. The command, project name, filenames, logs, and outputs are never workflow inputs or plaintext Actions artifacts. The release and its tag are deleted after successful retrieval. GitHub can observe timing, duration, ciphertext sizes, and job existence. GitHub necessarily receives plaintext inside its runner. A compromised worker identity can decrypt retained request ciphertext; it cannot decrypt job results. This design does not protect against GitHub, a compromised runner image/action, malicious dependencies, or traffic/size analysis.

The workflow deliberately does not stream the command output. Download, decrypt, execute, encrypt, and upload are separate Actions steps. The project command runs only after the step containing the worker private key has exited, and before the step containing the upload token starts. Checkout credentials are not persisted. Command execution uses an argument vector without a shell. Plaintext lives only under the runner temporary directory and is removed in an `always()` cleanup step; runner teardown is the final cleanup boundary.

## Worker setup

Use this repository (or a fork) as the dedicated worker.

1. Build the CLI locally: `go install github.com/adrielGGmotion/actions-cli@latest` (until a release exists, clone and run `go build -o remote .`).
2. Run `remote worker-keygen` once.
3. Put the private line in the worker repository secret named `REMOTE_AGE_IDENTITY`.
4. Put the corresponding public recipient in each project's `.remote.yml`.

Do not commit the private identity. Rotate it by replacing the secret and project recipients; in-flight jobs using the old recipient will fail.

```yaml
repository: adrielGGmotion/actions-cli
recipient: age1replace_with_worker_recipient

outputs:
  - app/build/outputs/**
  - build/reports/**

cache:
  - ~/.gradle/caches
  - ~/.gradle/wrapper

exclude:
  - some-large-dir/**
```

Authentication order is `GH_TOKEN`, `GITHUB_TOKEN`, then `gh auth token`. Tokens are never persisted or printed. For a fine-grained PAT, grant the worker repository **Actions: write** and **Contents: write**. Contents write is required for temporary releases/assets and tag cleanup; Actions write is required to dispatch and optionally cancel jobs.

## Workspace rules

The snapshot is based on `git ls-files -co --exclude-standard`, so it includes tracked, staged, unstaged, and non-ignored untracked files. `.git`, ignored files, common generated trees (`node_modules`, `target`, `.gradle`, `build`, and `dist`), and configured exclusions are omitted. Tracked files remain included unless covered by an automatic/configured exclusion. Workspace symlinks are rejected rather than followed. Filenames with spaces and Unicode are preserved by NUL-delimited Git enumeration and tar headers.

Exactly this is transmitted, encrypted: selected regular files and modes; exact command argv; project-relative working directory; output/cache patterns; and an ephemeral reply public key. Repository name and command are not placed in public job metadata.

Outputs are accepted only when they match a configured pattern. Archive traversal, absolute paths, non-regular entries, symlink parents, and symlink destinations are rejected. Files are written to temporary siblings and atomically renamed. Existing configured output files may be replaced; unrelated paths cannot be returned by a conforming client.

## Current MVP limitations

- `cache` is parsed and encrypted but not yet connected to `actions/cache`; no workspace cache is persisted.
- `status`, `logs`, and an explicit `cancel` subcommand are not implemented yet. Ctrl+C cancels the matching runner using its opaque workflow run title and attempts release cleanup.
- Releases deleted through the API may remain in GitHub backups according to GitHub's retention practices; they contain only age ciphertext.
- The client currently has no configurable total workspace/output size limit beyond per-entry safety limits and GitHub's release asset limits.
- If the runner is cancelled before uploading a result, the client waits until interrupted. A later version should correlate and poll the workflow run state.
- A network failure during final cleanup may leave an opaque ciphertext release/tag behind; retry cleanup manually after connectivity returns.
- Cache poisoning protections will be designed when cache support is implemented. Cache keys must be scoped to the worker owner and a non-secret project discriminator, and forked/untrusted runs must not write trusted caches.

## Development

```console
go test ./...
go vet ./...
```

The workflow is generic and does not need project-specific edits. Repository content is untrusted code and receives the worker job token, so use a dedicated repository with least-privilege permissions and never store unrelated secrets there.
