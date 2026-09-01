// release builds rnv3's release set and, optionally, ships it to a Pi.
// It is the one build tool for every platform (Windows, macOS, Linux): the
// only prerequisite is the Go toolchain that runs it.
//
//	go run ./tools/release                      dist/rnv3 + dist/rnv3-migrate (linux/arm64)
//	                                            and dist/rnv3-setup[.exe] for this PC
//	go run ./tools/release -target darwin/arm64 the setup tool for another OS/arch
//	go run ./tools/release -deploy raspinoaa    dev fast path: build the Pi binaries,
//	                                            copy them over ssh and run install.sh
//
// deploy/release.{ps1,sh} and deploy/deploy.{ps1,sh} are thin wrappers that
// find Go when it is not on PATH and run this program.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// payloadFiles are the Pi-side files embedded in rnv3-setup, relative to
// the repo root.
var payloadFiles = []string{
	"dist/rnv3",
	"dist/rnv3-migrate",
	"deploy/install.sh",
	"deploy/cutover.sh",
	"deploy/rnv3.service",
	"config.example.yaml",
}

func main() {
	arch := flag.String("arch", "arm64", "GOARCH of the Pi binaries: arm64 for Pi 3/4/5 64-bit, amd64 for x64 PCs")
	target := flag.String("target", "", "GOOS/GOARCH to build rnv3-setup for (default: this PC), e.g. darwin/arm64")
	deploy := flag.String("deploy", "", "Pi hostname or IP: build the Pi binaries, copy them over ssh and run install.sh (skips the setup tool)")
	user := flag.String("user", "pi", "SSH user for -deploy")
	installArgs := flag.String("install-args", "", "extra arguments for install.sh on the Pi, e.g. \"--skip-builds --no-start\"")
	noInstall := flag.Bool("no-install", false, "with -deploy: only copy the files to /tmp/rnv3-deploy, do not install")
	flag.Parse()

	if err := run(*arch, *target, *deploy, *user, *installArgs, *noInstall); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(arch, target, deploy, user, installArgs string, noInstall bool) error {
	repo, err := repoRoot()
	if err != nil {
		return err
	}
	goBin, err := findGo()
	if err != nil {
		return err
	}
	version := gitVersion(repo)
	dist := filepath.Join(repo, "dist")
	if err := os.MkdirAll(dist, 0o755); err != nil {
		return err
	}
	b := &builder{repo: repo, goBin: goBin}

	fmt.Printf("==> Using %s\n", goBin)
	fmt.Printf("==> Building rnv3 + rnv3-migrate %s for linux/%s\n", version, arch)
	if err := b.build("linux", arch, "./cmd/rnv3", filepath.Join(dist, "rnv3"), "-X main.version="+version); err != nil {
		return fmt.Errorf("rnv3 build failed: %w", err)
	}
	if err := b.build("linux", arch, "./tools/migrate", filepath.Join(dist, "rnv3-migrate"), ""); err != nil {
		return fmt.Errorf("rnv3-migrate build failed: %w", err)
	}

	if deploy != "" {
		return deployTo(repo, deploy, user, installArgs, noInstall, version)
	}

	fmt.Println("==> Assembling the rnv3-setup payload")
	payload := filepath.Join(repo, "cmd", "rnv3-setup", "payload")
	for _, f := range payloadFiles {
		src := filepath.Join(repo, filepath.FromSlash(f))
		if err := copyFile(src, filepath.Join(payload, filepath.Base(src))); err != nil {
			return err
		}
	}

	goos, goarch := runtime.GOOS, runtime.GOARCH
	out := filepath.Join(dist, "rnv3-setup")
	if target != "" {
		tos, tarch, ok := strings.Cut(target, "/")
		if !ok || tos == "" || tarch == "" {
			return fmt.Errorf("-target must be GOOS/GOARCH, e.g. darwin/arm64 (got %q)", target)
		}
		goos, goarch = tos, tarch
		if goos != runtime.GOOS || goarch != runtime.GOARCH {
			out = filepath.Join(dist, "rnv3-setup-"+goos+"-"+goarch)
		}
	}
	if goos == "windows" {
		out += ".exe"
	}
	fmt.Printf("==> Building rnv3-setup %s for %s/%s\n", version, goos, goarch)
	ldflags := "-X main.version=" + version + " -X main.payloadArch=" + arch
	if err := b.build(goos, goarch, "./cmd/rnv3-setup", out, ldflags); err != nil {
		return fmt.Errorf("rnv3-setup build failed: %w", err)
	}

	fmt.Println("==> Done:")
	entries, _ := os.ReadDir(dist)
	for _, e := range entries {
		if info, err := e.Info(); err == nil && !e.IsDir() {
			fmt.Printf("    %-26s %12s bytes\n", e.Name(), commas(info.Size()))
		}
	}
	rel, _ := filepath.Rel(repo, out)
	switch {
	case goos != runtime.GOOS || goarch != runtime.GOARCH:
		fmt.Printf("Copy %s to a %s/%s machine and run it there.\n", filepath.ToSlash(rel), goos, goarch)
	case runtime.GOOS == "windows":
		fmt.Printf("Run: .\\%s\n", rel)
	default:
		fmt.Printf("Run: ./%s\n", filepath.ToSlash(rel))
	}
	return nil
}

type builder struct {
	repo, goBin string
}

// build cross-compiles pkg for goos/goarch into out. Every build is
// CGO_ENABLED=0: rnv3 is pure Go, and this keeps macOS builds free of Xcode.
func (b *builder) build(goos, goarch, pkg, out, ldflags string) error {
	args := []string{"build", "-trimpath", "-ldflags", strings.TrimSpace("-s -w " + ldflags), "-o", out, pkg}
	cmd := exec.Command(b.goBin, args...)
	cmd.Dir = b.repo
	cmd.Env = append(os.Environ(), "GOOS="+goos, "GOARCH="+goarch, "CGO_ENABLED=0")
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	return cmd.Run()
}

// deployTo mirrors the old deploy.ps1: stage in /tmp/rnv3-deploy on the Pi,
// fix the exec bits scp drops, run install.sh. It shells out to the ssh/scp
// that Windows 10+, macOS and Linux all ship, so key auth and password
// prompts behave exactly as in a terminal.
func deployTo(repo, host, user, installArgs string, noInstall bool, version string) error {
	target := user + "@" + host
	dist := filepath.Join(repo, "dist")
	fmt.Printf("==> Copying to %s:/tmp/rnv3-deploy\n", target)
	if err := sh("ssh", target, "rm -rf /tmp/rnv3-deploy && mkdir -p /tmp/rnv3-deploy/deploy"); err != nil {
		return fmt.Errorf("ssh failed: %w", err)
	}
	if err := sh("scp", filepath.Join(dist, "rnv3"), filepath.Join(dist, "rnv3-migrate"),
		filepath.Join(repo, "config.example.yaml"), target+":/tmp/rnv3-deploy/"); err != nil {
		return fmt.Errorf("scp failed: %w", err)
	}
	if err := sh("scp", filepath.Join(repo, "deploy", "install.sh"), filepath.Join(repo, "deploy", "cutover.sh"),
		filepath.Join(repo, "deploy", "rnv3.service"), target+":/tmp/rnv3-deploy/deploy/"); err != nil {
		return fmt.Errorf("scp failed: %w", err)
	}
	if err := sh("ssh", target, "chmod +x /tmp/rnv3-deploy/deploy/*.sh"); err != nil {
		return fmt.Errorf("chmod failed: %w", err)
	}
	install := strings.TrimSpace("cd /tmp/rnv3-deploy && ./deploy/install.sh " + installArgs + " ./rnv3")
	if noInstall {
		fmt.Printf("==> Copied. Install with: ssh %s '%s'\n", target, install)
		return nil
	}
	fmt.Println("==> Installing on the Pi")
	if err := sh("ssh", "-t", target, install); err != nil {
		return fmt.Errorf("install failed: %w", err)
	}
	fmt.Printf("==> Deployed %s. Log: ssh %s journalctl -u rnv3 -f\n", version, target)
	return nil
}

func sh(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}

// repoRoot walks up from the working directory to the go.mod.
func repoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New("run this from inside the rnv3 repository (no go.mod found)")
		}
		dir = parent
	}
}

// findGo prefers the toolchain that is running this program (GOROOT), so the
// wrappers' Go lookup carries through even when go is not on PATH.
func findGo() (string, error) {
	exe := "go"
	if runtime.GOOS == "windows" {
		exe += ".exe"
	}
	if root := runtime.GOROOT(); root != "" {
		if p := filepath.Join(root, "bin", exe); fileExists(p) {
			return p, nil
		}
	}
	if p, err := exec.LookPath("go"); err == nil {
		return p, nil
	}
	return "", errors.New("Go was not found. Install it from https://go.dev/dl/ (or put go on your PATH)")
}

func gitVersion(repo string) string {
	cmd := exec.Command("git", "describe", "--tags", "--always", "--dirty")
	cmd.Dir = repo
	out, err := cmd.Output()
	if v := strings.TrimSpace(string(out)); err == nil && v != "" {
		return v
	}
	return "dev"
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

func commas(n int64) string {
	s := fmt.Sprint(n)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}
