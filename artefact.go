package main

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime/debug"
	"strings"

	"go.moehl.dev/go-update/internal"

	"golang.org/x/mod/semver"
)

type Artefact interface {
	// ModulePath to look up available versions of the module.
	ModulePath() string

	// InstallPath is the module path plus the path to the main package inside
	// the module. It can be used by `go install` together with a version.
	InstallPath() string

	// InstalledVersion is the currently installed version of the binary.
	InstalledVersion() string

	// TargetVersion that should be installed.
	TargetVersion() string

	// NeedsUpdate returns whether the artefact should be updated.
	NeedsUpdate() bool

	// Update installs the target version of the binary.
	Update() error
}

func NewArtefact(bi *debug.BuildInfo) (Artefact, error) {
	if bi == nil {
		return nil, fmt.Errorf("build info is nil")
	}

	if bi.Main.Path == "golang.org/dl" {
		return newGoToolchain(*bi)
	} else {
		return newBinary(*bi)
	}
}

type binary struct {
	debug.BuildInfo

	targetVersion string
	args          []string
	env           []string
}

func newBinary(bi debug.BuildInfo) (Artefact, error) {
	versions, err := internal.ListVersions(bi.Main.Path)
	if err != nil {
		return nil, err
	}

	if len(versions) == 0 {
		slog.Warn("go list did not return any version, using 'latest'", "module", bi.Main.Path)
		return &binary{
			BuildInfo:     bi,
			targetVersion: "latest",
		}, nil
	}

	// Select the most recent valid, non-prerelease version.
	var v string
	for i := len(versions) - 1; i >= 0; i-- {
		v = versions[i]

		if !semver.IsValid(v) {
			continue
		} else if semver.Prerelease(v) != "" {
			continue
		} else {
			break
		}
	}

	return &binary{
		BuildInfo:     bi,
		targetVersion: v,
	}, nil
}

func (b *binary) ModulePath() string       { return b.Main.Path }
func (b *binary) InstallPath() string      { return b.Path }
func (b *binary) InstalledVersion() string { return b.Main.Version }
func (b *binary) TargetVersion() string    { return b.targetVersion }
func (b *binary) NeedsUpdate() bool        { return b.targetVersion != b.InstalledVersion() }
func (b *binary) Update() error            { return internal.Install(b.InstallPath(), b.TargetVersion()) }

type goToolchain struct {
	executablePath   string
	installedVersion string
	targetVersion    string
}

func newGoToolchain(bi debug.BuildInfo) (Artefact, error) {
	a := &goToolchain{}

	if bi.Main.Path != a.ModulePath() {
		return nil, fmt.Errorf("build info is not a go toolchain")
	}

	a.installedVersion = path.Base(bi.Path)

	res, err := client.Get("https://go.dev/VERSION?m=text")
	if err != nil {
		return nil, err
	}
	defer func() { _ = res.Body.Close() }()

	body, err := io.ReadAll(res.Body)
	if err != nil {
		return nil, err
	}

	a.targetVersion = strings.SplitN(string(body), "\n", 2)[0]

	return a, nil
}

func (b *goToolchain) ModulePath() string       { return "golang.org/dl" }
func (b *goToolchain) InstallPath() string      { return path.Join(b.ModulePath(), b.targetVersion) }
func (b *goToolchain) InstalledVersion() string { return b.installedVersion }
func (b *goToolchain) TargetVersion() string    { return b.targetVersion }
func (b *goToolchain) NeedsUpdate() bool        { return b.TargetVersion() != b.InstalledVersion() }

func (b *goToolchain) Update() error {
	err := internal.Install(b.InstallPath(), "latest")
	if err != nil {
		return err
	}

	err = exec.Command(b.TargetVersion(), "download").Run()
	if err != nil {
		return err
	}

	err = os.Remove(filepath.Join(goBin, b.installedVersion))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}

	err = os.Remove(filepath.Join(goBin, "go"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}

	err = os.Symlink(filepath.Join(goBin, b.targetVersion), filepath.Join(goBin, "go"))
	if err != nil {
		return err
	}

	return nil
}
