#!/usr/bin/env bash
set -euo pipefail
# Run in the official Arch base-devel container, with dist mounted at /release.
work=$(mktemp -d)
useradd -m builder
cp /release/distribution/aur/PKGBUILD /release/distribution/aur/.SRCINFO "$work/"
cp /release/peep_*_linux_amd64.tar.gz "$work/"
chown -R builder:builder "$work"
cd "$work"
su builder -c 'makepkg --printsrcinfo' > actual.SRCINFO
diff -u .SRCINFO actual.SRCINFO
su builder -c 'makepkg --nodeps --noconfirm'
# Test the binary and installed layout without downloading media dependencies.
pacman -Udd --noconfirm ./*.pkg.tar.zst
peep --version
printf 'flowchart LR\nA[peep]-->B[Mermaid]\n' | peep __peep_mermaid_png_v1 > diagram.png
test "$(od -An -tx1 -N8 diagram.png | tr -d ' \n')" = 89504e470d0a1a0a
test -f /usr/share/licenses/peep-tui-bin/LICENSE
test -f /usr/share/peep/examples/night-shift/00-start-here.md
printf 'Arch package install and embedded Mermaid passed.\n'
