package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime/debug"
	"strings"
	"time"
	"unicode"
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
	// See https://go.dev/ref/mod#goproxy-protocol for the protocol.
	// TODO: walk all possible proxies, not just the default one hard-coded.
	latestUrl := fmt.Sprintf("https://proxy.golang.org/%s/@latest", escapeModuleName(bi.Main.Path))
	slog.Debug("making request", "method", http.MethodGet, "url", latestUrl)
	res, err := client.Get(latestUrl)
	if err != nil {
		return nil, err
	}
	if res.StatusCode != http.StatusOK {
		// We don't expect very long error messages
		errMsg, _ := io.ReadAll(io.LimitReader(res.Body, 512))
		return nil, fmt.Errorf("get latest version: got %s: %s", res.Status, errMsg)
	}

	var latestVersion struct {
		Version string
		Time    time.Time
	}
	err = json.NewDecoder(res.Body).Decode(&latestVersion)
	if err != nil {
		return nil, err
	}

	return &binary{
		BuildInfo:     bi,
		targetVersion: latestVersion.Version,
	}, nil
}

func (b *binary) ModulePath() string       { return b.Main.Path }
func (b *binary) InstallPath() string      { return b.Path }
func (b *binary) InstalledVersion() string { return b.Main.Version }
func (b *binary) TargetVersion() string    { return b.targetVersion }
func (b *binary) NeedsUpdate() bool        { return b.targetVersion != b.InstalledVersion() }
func (b *binary) Update() error            { return install(b.InstallPath(), b.TargetVersion()) }

// escapeModuleName to prepare the string for usage in accordance with the go module proxy protocol.
// See: https://go.dev/ref/mod#goproxy-protocol
func escapeModuleName(in string) string {
	buf := bytes.NewBuffer(make([]byte, 0, len(in)))

	for _, r := range in {
		if unicode.IsUpper(r) {
			buf.WriteRune('!')
			buf.WriteRune(unicode.ToLower(r))
		} else {
			buf.WriteRune(r)

		}
	}

	return buf.String()
}

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
	err := install(b.InstallPath(), "latest")
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
