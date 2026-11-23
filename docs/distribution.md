# Distribution

peep ships one Go executable, with Mermaid built in, for macOS and Linux on
arm64 and x86-64. Releases include the MIT license and third-party notices.
FFmpeg and mpv provide media decoding and playback.

| Channel | Install command | Media dependencies |
| --- | --- | --- |
| Homebrew tap | `brew install lancekrogers/tap/peep` | Installed by Homebrew |
| npm | `npm install -g @obedience-corp/peep` | Install FFmpeg and mpv separately |
| AUR | `yay -S peep-tui-bin` | Installed by the AUR helper |

The executable is always `peep`. The unscoped npm name `peep` and AUR
names `peep`/`peep-bin` belong to unrelated projects.

## Build and verify

```sh
just check
just distribution 0.2.0
just test-distribution
just test-aur                       # Docker: actual Arch build/install
```

Choose an unused stable version for an actual release. Generation refuses to
replace `dist/distribution`; remove that generated directory explicitly before
rebuilding. Package metadata lives in `packaging/metadata.json`.

The generator verifies all four archive checksums, then creates:

- `dist/distribution/homebrew/peep.rb`
- `dist/distribution/aur/PKGBUILD` and `.SRCINFO`
- `dist/distribution/npm/` with a command package and four native packages
- `dist/distribution/npm-tarballs/` with publishable npm tarballs

The npm launcher uses exact-version optional dependencies selected by npm for
the host OS and CPU. It has no install scripts, network downloads, or compiler
requirements. Node 22 or newer is required for this installation channel.
The npm smoke check installs tarballs offline into a temporary prefix and
verifies both `--version` and Mermaid PNG output. It does not modify the user's
global installation.

## One-time publishing setup

The existing `@obedience-corp` npm organization is used independently of the
personal GitHub repository. The existing AUR maintainer account is `obediencecorp`.

1. Make the repository public when ready to launch. Package publishing refuses
   a private repository because Homebrew/AUR download URLs must be public.
2. Confirm ownership of the npm scope `@obedience-corp`. Publish all four native
   packages before the command package. For the initial publication, use an
   authenticated npm CLI locally or a short-lived `NPM_TOKEN` GitHub environment
   secret with package creation rights. Follow npm's account/2FA requirements.
3. Configure an npm trusted publisher on **each of the five packages** for
   repository `lancekrogers/peep`, workflow `publish.yml`, environment
   `distribution`. Subsequent publication uses GitHub OIDC; remove the initial
   token. The workflow uses Node 24 with its bundled modern npm.
4. In GitHub's `distribution` environment, add `HOMEBREW_TAP_TOKEN` with contents
   and pull-request write access to `lancekrogers/homebrew-tap`.
5. Use an SSH key registered to the existing AUR account `obediencecorp`
   with write access to `peep-tui-bin`. The existing AUR key can be reused.
   Add environment secrets `AUR_SSH_KEY`, `AUR_EMAIL`, and `AUR_KNOWN_HOSTS`.
   Verify the AUR SSH host fingerprint through Arch's published information
   before storing its known-hosts entry. The workflow never accepts a new host
   key automatically.

The existing personal Homebrew tap is used; no Homebrew/core submission is
needed. Generated formula updates are opened as pull requests and merged by
the publishing workflow. Application changes still go through normal PR review.

## Release

After merging the changes to release, run from the repository:

```sh
just release          # next patch: 0.2.0 -> 0.2.1
just release minor    # next minor: 0.2.0 -> 0.3.0
just release major    # next major: 0.2.0 -> 1.0.0
```

CI chooses the next version from the highest stable GitHub tag, including tags
beyond the first API page. Prerelease and demo tags are ignored. Minor and major
bumps reset the lower components; with no stable tags, patch starts at `0.0.1`.
Release runs are serialized so version selection happens in CI, not on your
machine. An explicit version is still available with `just release 0.2.1`.

This dispatches CI against remote `main`,
not your local working copy. GitHub CLI authentication is required locally;
no local npm login, npm confirmation, or manual SSH step is needed per release.
The workflow:

1. Builds all four archives and generates every channel from those archives.
2. Tests npm installation and embedded Mermaid on Linux and macOS, and the Arch package.
3. Tags the exact main commit tested, without moving an existing version tag.
4. Publishes the archives, npm tarballs, formula, AUR metadata, and checksums.
5. Starts **publish packages** using the saved credentials and npm trusted publishing.
   npm publishes the native packages before the launcher; Homebrew creates and
   merges the generated formula PR; AUR pushes the recipe and matching `.SRCINFO`.

The command starts the release; it does not wait for completion. Follow **release**
and then **publish packages** in [Actions](https://github.com/lancekrogers/peep/actions).
A green release build alone does not mean every package channel has finished.
A manually pushed stable version tag also runs this build-and-publish flow.

If building fails, rerun the failed jobs. If a publishing channel fails after the
GitHub release exists, retry it without rebuilding or creating another version:

```sh
gh workflow run publish.yml --repo lancekrogers/peep --ref main \
  -f version=0.2.1 -f channel=all
```

Use `npm`, `homebrew`, or `aur` instead of `all` to retry one channel. A blocked tap
merge fails visibly rather than bypassing repository protections. Credentials
only need attention if revoked or expired; npm trusted publishers must remain
configured for `publish.yml` and the `distribution` environment.

After the first automated release, verify fresh public installs with
`peep --version` and `peep --doctor`. npm installations need FFmpeg/mpv installed
separately; see [installation](install.md).

Published versions are immutable. If a channel fails, resolve its credential
or package issue and rerun that channel. If npm publication stopped halfway,
the publisher verifies already-published packages against the release tarball
integrity before skipping them; do not replace a published version with different bytes.

References: [npm trusted publishers](https://docs.npmjs.com/trusted-publishers/),
[Homebrew taps](https://docs.brew.sh/How-to-Create-and-Maintain-a-Tap),
[AUR submission guidelines](https://wiki.archlinux.org/title/AUR_submission_guidelines).
