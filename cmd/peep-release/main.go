// peep-release derives package-manager inputs from the same verified release archives.
package main

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

type metadata struct {
	Repository string `json:"repository"`
	NPMName    string `json:"npmName"`
	AURName    string `json:"aurName"`
	License    string `json:"license"`
}
type target struct{ OS, Arch, NodeArch, ArchLinux string }

var targets = []target{{"darwin", "arm64", "arm64", ""}, {"darwin", "amd64", "x64", ""}, {"linux", "arm64", "arm64", "aarch64"}, {"linux", "amd64", "x64", "x86_64"}}
var stableVersion = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)

func main() {
	version := flag.String("version", "", "stable version, without v")
	archives := flag.String("archives", "dist", "directory containing all four archives and SHA256 files")
	output := flag.String("out", "dist/distribution", "generated package definitions")
	flag.Parse()
	if err := generate(*version, *archives, *output); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func generate(version, archives, output string) error {
	if !stableVersion.MatchString(version) {
		return fmt.Errorf("release version must be stable MAJOR.MINOR.PATCH, without v")
	}
	data, err := os.ReadFile("packaging/metadata.json")
	if err != nil {
		return err
	}
	var m metadata
	if err = json.Unmarshal(data, &m); err != nil {
		return err
	}
	if !regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`).MatchString(m.Repository) || !regexp.MustCompile(`^@[a-z0-9_-]+/[a-z0-9_-]+$`).MatchString(m.NPMName) || !regexp.MustCompile(`^[a-z0-9][a-z0-9-]*-bin$`).MatchString(m.AURName) {
		return fmt.Errorf("invalid distribution package identifiers")
	}
	if m.License == "" {
		return fmt.Errorf("choose the project license in packaging/metadata.json and add LICENSE before generating distribution packages")
	}
	if !regexp.MustCompile(`^[A-Za-z0-9.-]+$`).MatchString(m.License) {
		return fmt.Errorf("invalid SPDX license identifier")
	}
	license, err := os.ReadFile("LICENSE")
	if err != nil {
		return fmt.Errorf("read project license: %w", err)
	}
	// Build in a fresh sibling directory: failed validation never exposes partial output.
	if err = os.MkdirAll(filepath.Dir(output), 0755); err != nil {
		return err
	}
	stage, err := os.MkdirTemp(filepath.Dir(output), ".distribution-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	sums := map[string]string{}
	deps := map[string]string{}
	baseURL := "https://github.com/" + m.Repository + "/releases/download/v" + version + "/"
	var checksums strings.Builder
	for _, t := range targets {
		name := fmt.Sprintf("peep_%s_%s_%s.tar.gz", version, t.OS, t.Arch)
		sum, err := verifiedDigest(filepath.Join(archives, name))
		if err != nil {
			return err
		}
		sums[t.OS+"-"+t.Arch] = sum
		fmt.Fprintf(&checksums, "%s  %s\n", sum, name)
		packageName := m.NPMName + "-" + t.OS + "-" + t.NodeArch
		deps[packageName] = version
		dir := filepath.Join(stage, "npm", t.OS+"-"+t.NodeArch)
		if err = os.MkdirAll(dir, 0755); err != nil {
			return err
		}
		if err = extractNative(filepath.Join(archives, name), dir); err != nil {
			return err
		}
		p := packageManifest(m, packageName, version)
		p["os"] = []string{t.OS}
		p["cpu"] = []string{t.NodeArch}
		p["files"] = []string{"bin/peep", "LICENSE", "licenses"}
		p["description"] = "Native peep executable for " + t.OS + " " + t.NodeArch
		if err = writeJSON(filepath.Join(dir, "package.json"), p); err != nil {
			return err
		}
	}
	wrapper := filepath.Join(stage, "npm", "peep")
	if err = os.MkdirAll(wrapper, 0755); err != nil {
		return err
	}
	p := packageManifest(m, m.NPMName, version)
	p["bin"] = map[string]string{"peep": "peep.cjs"}
	p["engines"] = map[string]string{"node": ">=22"}
	p["optionalDependencies"] = deps
	p["files"] = []string{"peep.cjs", "LICENSE"}
	p["description"] = "Terminal file explorer with image, video, music, Markdown and Mermaid previews"
	if err = writeJSON(filepath.Join(wrapper, "package.json"), p); err != nil {
		return err
	}
	for _, file := range []string{"peep.cjs", "README.md"} {
		b, err := os.ReadFile(filepath.Join("packaging/npm", file))
		if err != nil {
			return err
		}
		mode := os.FileMode(0644)
		if file == "peep.cjs" {
			mode = 0755
		}
		if err = os.WriteFile(filepath.Join(wrapper, file), b, mode); err != nil {
			return err
		}
	}
	if err = os.WriteFile(filepath.Join(wrapper, "LICENSE"), license, 0644); err != nil {
		return err
	}
	for name, content := range map[string]string{"checksums.txt": checksums.String(), "homebrew/peep.rb": formula(m, version, baseURL, sums), "aur/PKGBUILD": pkgbuild(m, version, baseURL, sums), "aur/.SRCINFO": srcinfo(m, version, baseURL, sums)} {
		path := filepath.Join(stage, name)
		if err = os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			return err
		}
		if err = os.WriteFile(path, []byte(content), 0644); err != nil {
			return err
		}
	}
	if _, err = os.Stat(output); err == nil {
		return fmt.Errorf("output %s already exists; remove it explicitly before regenerating", output)
	} else if !os.IsNotExist(err) {
		return err
	}
	return os.Rename(stage, output)
}
func packageManifest(m metadata, name, version string) map[string]any {
	return map[string]any{"name": name, "version": version, "license": m.License, "repository": map[string]string{"type": "git", "url": "git+https://github.com/" + m.Repository + ".git"}, "homepage": "https://github.com/" + m.Repository, "publishConfig": map[string]string{"access": "public"}}
}
func writeJSON(path string, value any) error {
	b, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0644)
}
func verifiedDigest(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err = io.Copy(h, f); err != nil {
		return "", err
	}
	sum := hex.EncodeToString(h.Sum(nil))
	sidecar, err := os.ReadFile(path + ".sha256")
	if err != nil {
		return "", err
	}
	fields := strings.Fields(string(sidecar))
	if len(fields) != 2 || fields[0] != sum || fields[1] != filepath.Base(path) {
		return "", fmt.Errorf("checksum mismatch for %s", path)
	}
	return sum, nil
}
func extractNative(archive, dest string) error {
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	required := map[string]bool{"bin/peep": false, "LICENSE": false, "licenses/MMDG-LICENSE.txt": false, "licenses/MMDG-NOTICES.txt": false}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if _, ok := required[h.Name]; !ok {
			continue
		}
		if required[h.Name] || h.Typeflag != tar.TypeReg || h.Size <= 0 || h.Size > 256<<20 {
			return fmt.Errorf("invalid package member %s", h.Name)
		}
		mode := os.FileMode(0644)
		if h.Name == "bin/peep" {
			mode = 0755
		}
		name := filepath.Join(dest, filepath.FromSlash(h.Name))
		if err = os.MkdirAll(filepath.Dir(name), 0755); err != nil {
			return err
		}
		out, err := os.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(out, tr)
		closeErr := out.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
		required[h.Name] = true
	}
	for name, found := range required {
		if !found {
			return fmt.Errorf("archive missing %s", name)
		}
	}
	return nil
}
func formula(m metadata, v, url string, sums map[string]string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Generated by peep-release; do not edit checksums by hand.\nclass Peep < Formula\n  desc \"Terminal file explorer with media, Markdown and Mermaid previews\"\n  homepage \"https://github.com/%s\"\n  version \"%s\"\n  license \"%s\"\n\n", m.Repository, v, m.License)
	for _, osname := range []string{"darwin", "linux"} {
		block := "on_macos"
		if osname == "linux" {
			block = "on_linux"
		}
		fmt.Fprintf(&b, "  %s do\n", block)
		for _, arch := range []string{"arm64", "amd64"} {
			condition := "on_arm"
			if arch == "amd64" {
				condition = "on_intel"
			}
			fmt.Fprintf(&b, "    %s do\n      url \"%speep_%s_%s_%s.tar.gz\"\n      sha256 \"%s\"\n    end\n", condition, url, v, osname, arch, sums[osname+"-"+arch])
		}
		b.WriteString("  end\n\n")
	}
	b.WriteString(`  depends_on "ffmpeg"
  depends_on "mpv"

  def install
    bin.install "bin/peep"
    pkgshare.install "examples", "docs", "licenses", "LICENSE"
  end

  test do
    assert_match version.to_s, shell_output("#{bin}/peep --version")
    png = pipe_output("#{bin}/peep __peep_mermaid_png_v1", "flowchart LR\nA[peep]-->B[Mermaid]\n", 0)
    assert_equal "\x89PNG\r\n\x1a\n".b, png.byteslice(0, 8).b
  end
end
`)
	return b.String()
}
func pkgbuild(m metadata, v, url string, sums map[string]string) string {
	return fmt.Sprintf(`# Generated by peep-release.
pkgname=%s
pkgver=%s
pkgrel=1
pkgdesc='Terminal file explorer with media, Markdown and Mermaid previews'
arch=('x86_64' 'aarch64')
url='https://github.com/%s'
license=('%s')
depends=('ffmpeg' 'mpv')
conflicts=('peep' 'peep-bin' 'peep-tui')
options=('!strip')
source_x86_64=('%speep_%s_linux_amd64.tar.gz')
sha256sums_x86_64=('%s')
source_aarch64=('%speep_%s_linux_arm64.tar.gz')
sha256sums_aarch64=('%s')

package() {
  install -Dm755 "$srcdir/bin/peep" "$pkgdir/usr/bin/peep"
  install -Dm644 "$srcdir/LICENSE" "$pkgdir/usr/share/licenses/$pkgname/LICENSE"
  install -dm755 "$pkgdir/usr/share/peep"
  cp -a "$srcdir/docs" "$srcdir/examples" "$srcdir/licenses" "$pkgdir/usr/share/peep/"
}
`, m.AURName, v, m.Repository, m.License, url, v, sums["linux-amd64"], url, v, sums["linux-arm64"])
}
func srcinfo(m metadata, v, url string, sums map[string]string) string {
	return fmt.Sprintf(`pkgbase = %s
	pkgdesc = Terminal file explorer with media, Markdown and Mermaid previews
	pkgver = %s
	pkgrel = 1
	url = https://github.com/%s
	arch = x86_64
	arch = aarch64
	license = %s
	depends = ffmpeg
	depends = mpv
	conflicts = peep
	conflicts = peep-bin
	conflicts = peep-tui
	options = !strip
	source_x86_64 = %speep_%s_linux_amd64.tar.gz
	sha256sums_x86_64 = %s
	source_aarch64 = %speep_%s_linux_arm64.tar.gz
	sha256sums_aarch64 = %s

pkgname = %s
`, m.AURName, v, m.Repository, m.License, url, v, sums["linux-amd64"], url, v, sums["linux-arm64"], m.AURName)
}
