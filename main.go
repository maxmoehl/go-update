//go:build unix

package main

import (
	"debug/buildinfo"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

const (
	updateCmd       = "update"
	listCmd         = "list"
	goBinEnv        = "GOBIN"
	goPathEnv       = "GOPATH"
	goProxyEnv      = "GOPROXY"
	goMinVersionEnv = "GOMINVERSION"
	homeEnv         = "HOME"
)

var (
	client = http.DefaultClient

	logLevel = &slog.LevelVar{}

	// preserveSettings contains all keys from the build info that should be
	// carried over when generating the install-commands.
	preserveSettings = []string{"-tags"}

	// minGoVersion sets the minimum go version that the binaries have to be
	// built with. From go1.18 on the full build info is included, however, some
	// of it has been present with go1.17, and it might work with go1.17
	// binaries as well.
	minGoVersion = "go1.18"

	// goProxies contains the parsed list of the GOPROXY environment variable.
	// It honors the definition at
	// https://go.dev/ref/mod#environment-variables.
	goProxies = []string{"https://proxy.golang.org", "direct"}

	goBin string

	goCli string
)

type info struct {
	Version string
	Time    time.Time
}

type binary struct {
	name             string
	pkg              string
	installPath      string
	installedVersion string
	targetVersion    string
	args             []string
	env              []string
}

func (b binary) needsBump() bool {
	return b.installedVersion != b.targetVersion
}

func (b binary) versionBump() string {
	if b.needsBump() {
		return fmt.Sprintf("%s -> %s", b.installedVersion, b.targetVersion)
	} else {
		return b.installedVersion
	}
}

func (b binary) updateCmd() string {
	return fmt.Sprintf("go install %s@%s", b.installPath, b.targetVersion)
}

func (b binary) packageVersion() string {
	return fmt.Sprintf("%s@%s", b.installPath, b.targetVersion)
}

type binaries []binary

func (bs binaries) String() string {
	packageColumnLen := 0
	versionColumnLen := 0
	for _, b := range bs {
		if len(b.installPath) > packageColumnLen {
			packageColumnLen = len(b.installPath)
		}
		version := b.versionBump()
		if len(version) > versionColumnLen {
			versionColumnLen = len(version)
		}
	}

	builder := strings.Builder{}
	for _, b := range bs {
		builder.WriteString(b.installPath)
		builder.WriteString(strings.Repeat(" ", packageColumnLen-len(b.installPath)+1))
		version := b.versionBump()
		builder.WriteString(version)
		if b.needsBump() {
			builder.WriteString(strings.Repeat(" ", versionColumnLen-len(version)+1))
			builder.WriteString(b.updateCmd())
		}
		builder.WriteRune('\n')
	}

	return builder.String()
}

func init() {
	var err error
	defer func() {
		if err != nil {
			fmt.Printf("error: init: %s", err.Error())
			os.Exit(1) // exit code 1: error during init
		} else {
			slog.Debug("init done",
				goBinEnv, goBin,
				goMinVersionEnv, minGoVersion,
				"GOCLI", goCli,
				goProxyEnv, goProxies,
			)
		}
	}()

	logLevelEnv, ok := os.LookupEnv("LOG")
	if ok {
		err = logLevel.UnmarshalText([]byte(logLevelEnv))
		if err != nil {
			return
		}
	} else {
		logLevel.Set(slog.LevelError)
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: logLevel})))

	customMinGoVersion, ok := os.LookupEnv(goMinVersionEnv)
	if ok {
		minGoVersion = customMinGoVersion
	}

	goBin = os.Getenv(goBinEnv)
	if goBin == "" && os.Getenv(goPathEnv) != "" {
		goBin = filepath.Join(os.Getenv(goPathEnv), "bin")
	} else if goBin == "" && os.Getenv(homeEnv) != "" {
		goBin = filepath.Join(os.Getenv(homeEnv), "go", "bin")
	} else if goBin == "" {
		err = fmt.Errorf("unable to determine GOBIN: $GOBIN, $GOPATH and $HOME are not set")
		return
	}

	fileInfo, err := os.Stat(goBin)
	if err != nil {
		err = fmt.Errorf("stat $GOBIN (%s): %s", goBin, err.Error())
		return
	}
	if !fileInfo.IsDir() {
		err = fmt.Errorf("$GOBIN (%s) is not a directory", goBin)
		return
	}

	proxy, ok := os.LookupEnv(goProxyEnv)
	if ok {
		goProxies = strings.Split(proxy, ",")
	}

	goCli, err = exec.LookPath("go")
	if err != nil {
		err = fmt.Errorf("looking up go cli path: %w", err)
	}
}

type usageError error

var usageErr usageError

func main() {
	err := Main()
	if err != nil {
		if errors.As(err, &usageErr) {
			fmt.Printf("Usage: %s [ update (default) | list ]\n", os.Args[0])
		}
		fmt.Printf("error: main: %s\n", err.Error())
		os.Exit(2) // exit code 2: generic error during execution
	}
}

func Main() error {
	if len(os.Args) > 2 {
		return usageError(fmt.Errorf("error: only one argument can be provided"))
	}

	switch {
	case len(os.Args) < 2 || os.Args[1] == updateCmd:
		return update(goBin)
	case os.Args[1] == listCmd:
		bs, err := list(goBin)
		if err != nil {
			return err
		}
		fmt.Printf(bs.String())
		return nil
	default:
		return usageError(fmt.Errorf("unknown command '%s'", os.Args[1]))
	}
}

func update(binDir string) error {
	bs, err := list(binDir)
	if err != nil {
		return fmt.Errorf("udpate: %w", err)
	}

	for _, b := range bs {
		if !b.needsBump() {
			continue
		}
		err = execUpdate(b.packageVersion())
		if err != nil {
			return fmt.Errorf("update: %s: %w", b.installPath, err)
		}
	}

	return nil
}

func execUpdate(pkg string) error {
	cmd := exec.Cmd{
		Path:   goCli,
		Args:   []string{"go", "install", pkg},
		Stdout: os.Stdout,
		Stderr: os.Stderr,
	}

	slog.Debug("executing command", "cmd", cmd.String())

	return cmd.Run()
}

func list(binDir string) (binaries, error) {
	entries, err := fs.ReadDir(os.DirFS(binDir), ".")
	if err != nil {
		return nil, fmt.Errorf("list: %w", err)
	}

	var bs binaries
	for _, entry := range entries {
		if entry.IsDir() {
			slog.Warn("skipping directory", "name", entry.Name())
			continue
		}

		fileInfo, err := entry.Info()
		if err != nil {
			return nil, fmt.Errorf("list: %w", err)
		}

		if !executable(fileInfo.Mode()) {
			slog.Warn("skipping non-executable file", "name", entry.Name())
			continue
		}

		b, err := inspectBinary(filepath.Join(binDir, entry.Name()))
		if err != nil {
			slog.Warn("unable to inspect binary, skipping", "name", entry.Name(), "error", err.Error())
			continue
		}

		bs = append(bs, b)
	}

	return bs, nil
}

func inspectBinary(path string) (binary, error) {
	info, err := buildinfo.ReadFile(path)
	if err != nil {
		return binary{}, fmt.Errorf("udpate binary: unable to read buildinfo: %w", err)
	}
	if info.GoVersion < minGoVersion {
		return binary{}, fmt.Errorf("udpate binary: go version too old to update (%s)", info.GoVersion)
	}

	targetVersion, err := latest(info.Main.Path)
	if err != nil {
		return binary{}, fmt.Errorf("update binary: %w", err)
	}

	var args []string
	for _, setting := range info.Settings {
		if slices.Contains(preserveSettings, setting.Key) {
			args = append(args, fmt.Sprintf("%s=%s", setting.Key, setting.Value))
		}
	}

	return binary{
		name:             filepath.Base(path),
		pkg:              info.Main.Path,
		installPath:      info.Path,
		installedVersion: info.Main.Version,
		targetVersion:    targetVersion,
		args:             args,
		env:              nil,
	}, nil
}

// latest returns the latest version according to the GOPROXY.
func latest(module string) (string, error) {
	// TODO: walk all possible proxies, not just the default one hard-coded.
	latestUrl := fmt.Sprintf("https://proxy.golang.org/%s/@latest", module)
	res, err := client.Get(latestUrl)
	if err != nil {
		return "", fmt.Errorf("latest: %w", err)
	}

	var latestVersion info
	err = json.NewDecoder(res.Body).Decode(&latestVersion)
	if err != nil {
		return "", fmt.Errorf("latest: %w", err)
	}

	return latestVersion.Version, nil
}

func executable(mode os.FileMode) bool {
	return mode&0111 != 0
}
