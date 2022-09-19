package main

import (
	"debug/buildinfo"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
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

	printWarnings = false

	// preserveSettings contains all keys from the build info that should be
	// carried over when generating the install-commands.
	preserveSettings = []string{"-tags"}

	// minGoVersion sets the minimum go version that the binaries have to be
	// built with. From go1.18 on the full build info is included, however, some
	// of it has been present with go1.17, and it might work with go1.17
	// binaries as well.
	minGoVersion = "go1.18"

	// goProxies contains the parsed list of the GOPROXY environment variable.
	// It honors the definition at https://go.dev/ref/mod#environment-variables
	goProxies = []string{"https://proxy.golang.org", "direct"}

	goBin string
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

type binaries []binary

func (bs binaries) String() string {
	packageColumnLen := 0
	versionColumnLen := 0
	for _, b := range bs {
		if len(b.pkg) > packageColumnLen {
			packageColumnLen = len(b.pkg)
		}
		version := b.versionBump()
		if len(version) > versionColumnLen {
			versionColumnLen = len(version)
		}
	}

	builder := strings.Builder{}
	for _, b := range bs {
		builder.WriteString(b.pkg)
		builder.WriteString(strings.Repeat(" ", packageColumnLen-len(b.pkg)+1))
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
			fmt.Printf("error: %s\n", err.Error())
			os.Exit(1) // exit code 1: error during init
		}
	}()

	_, printWarnings = os.LookupEnv("PRINT_WARNINGS")
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
		err = fmt.Errorf("unable to stat GOBIN (%s): %s", goBin, err.Error())
		return
	}
	if !fileInfo.IsDir() {
		err = fmt.Errorf("error: init: GOBIN (%s) is not a directory", goBin)
		return
	}

	proxy, ok := os.LookupEnv(goProxyEnv)
	if ok {
		goProxies = strings.Split(proxy, ",")
	}
}

func main() {
	var err error
	defer func() {
		if err != nil {
			fmt.Printf("error: %s\n", err.Error())
			os.Exit(2) // exit code 2: generic error during execution
		}
	}()

	if len(os.Args) > 2 {
		err = fmt.Errorf("error: only one argument can be provided")
		return
	}

	fmt.Printf("using %s\n", goBin)

	if len(os.Args) < 2 || os.Args[1] == updateCmd {
		err = update(goBin)
	} else if os.Args[1] == listCmd {
		bs, err := list(goBin)
		if err != nil {
			return
		}
		fmt.Printf(bs.String())
	} else {
		err = fmt.Errorf("unknown command '%s'", os.Args[1])
	}
}

func update(binDir string) error {
	bs, err := list(binDir)
	if err != nil {
		return fmt.Errorf("udpate: %w", err)
	}

	for _, b := range bs {
		if b.needsBump() {
			fmt.Println(b.updateCmd())
		}
	}

	return nil
}

func list(binDir string) (binaries, error) {
	entries, err := fs.ReadDir(os.DirFS(binDir), ".")
	if err != nil {
		return nil, fmt.Errorf("list: %w", err)
	}

	var bs binaries
	for _, entry := range entries {
		if entry.IsDir() {
			warn(fmt.Sprintf("skipping directory '%s'", entry.Name()))
			continue
		}

		fileInfo, err := entry.Info()
		if err != nil {
			return nil, fmt.Errorf("list: %w", err)
		}

		if !executable(fileInfo.Mode()) {
			warn(fmt.Sprintf("skipping non-executable file '%s'", entry.Name()))
			continue
		}

		b, err := inspectBinary(filepath.Join(binDir, entry.Name()))
		if err != nil {
			warn(fmt.Sprintf("skipping '%s': %s", entry.Name(), err.Error()))
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
		if in(setting.Key, preserveSettings) {
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

func warn(msg string) {
	if printWarnings {
		fmt.Printf("warning: %s\n", msg)
	}
}

func executable(mode os.FileMode) bool {
	return mode&0111 != 0
}

func in[T comparable](v T, s []T) bool {
	for _, t := range s {
		if t == v {
			return true
		}
	}
	return false
}
