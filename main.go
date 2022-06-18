package main

import (
	"context"
	"debug/buildinfo"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/go-github/v45/github"
	"golang.org/x/mod/semver"
	"golang.org/x/oauth2"
)

var (
	client *github.Client

	printWarnings = false

	// preserveSettings contains all keys from the build info that should be
	// carried over when generating the install-commands.
	preserveSettings = []string{"-tags"}

	// minGoVersion sets the minimum go version that the binaries have to be
	// built with. From go1.18 on the full build info is included, however, some
	// of it has been present with go1.17, and it might work with go1.17
	// binaries as well.
	minGoVersion = "go1.18"
)

func init() {
	_, printWarnings = os.LookupEnv("PRINT_WARNINGS")
	customMinGoVersion, exists := os.LookupEnv("MIN_GO_VERSION")
	if exists {
		minGoVersion = customMinGoVersion
	}

	token := os.Getenv("GITHUB_TOKEN")
	if token == "" {
		panic("please set GITHUB_TOKEN")
	}

	ts := oauth2.StaticTokenSource(
		&oauth2.Token{AccessToken: token},
	)
	tc := oauth2.NewClient(context.Background(), ts)
	client = github.NewClient(tc)
}

func main() {
	err := update()
	if err != nil {
		fmt.Printf("error: %s\n", err.Error())
	}
}

func update() error {
	binDir, err := goBin()
	if err != nil {
		return fmt.Errorf("udpate: %w", err)
	}

	fileInfo, err := os.Stat(binDir)
	if err != nil {
		return fmt.Errorf("udpate: %w", err)
	}

	fmt.Printf("using %s\n", binDir)

	if !fileInfo.IsDir() {
		return fmt.Errorf("GOBIN (%s) is not a directory", binDir)
	}

	entries, err := fs.ReadDir(os.DirFS(binDir), ".")
	if err != nil {
		return fmt.Errorf("update: %w", err)
	}

	for _, entry := range entries {
		if entry.IsDir() {
			warn(fmt.Sprintf("skipping directory '%s'", entry.Name()))
			continue
		}

		fileInfo, err := entry.Info()
		if err != nil {
			return fmt.Errorf("update: %w", err)
		}

		if !executable(fileInfo.Mode()) {
			warn(fmt.Sprintf("skipping non-executable file '%s'", entry.Name()))
			continue
		}

		err = updateBin(filepath.Join(binDir, entry.Name()))
		if err != nil && printWarnings {
			warn(fmt.Sprintf("skipping '%s': %s", entry.Name(), err.Error()))
		}
	}

	return nil
}

func updateBin(path string) error {
	info, err := buildinfo.ReadFile(path)
	if err != nil {
		return fmt.Errorf("udpate binary: unable to read buildinfo: %w", err)
	}
	if info.GoVersion < minGoVersion {
		return fmt.Errorf("udpate binary: go version too old to update (%s)", info.GoVersion)
	}
	if !strings.HasPrefix(info.Main.Path, "github.com/") {
		return fmt.Errorf("udpate binary: only packages from github.com are supported as of now")
	}

	// 0: github.com; 1: owner; 2: repo name; 3+: path
	repositoryParts := strings.Split(info.Main.Path, "/")

	tags, res, err := client.Repositories.ListTags(context.Background(), repositoryParts[1], repositoryParts[2], nil)
	if err != nil {
		return fmt.Errorf("update binary: %w", err)
	}

	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("update binary: http request failed: %s", res.Status)
	}

	// we consider everything that can't be parsed or is installed as v0.0.0 as invalid
	// TODO: for modules that don't use tags, semver and modules we need to figure something out
	if !semver.IsValid(info.Main.Version) || semver.Compare(info.Main.Version, "v0.0.0") == 0 {
		return fmt.Errorf("udpate binary: invalid version installed: %s", info.Main.Version)
	}

	targetVersion := info.Main.Version
	for _, tag := range tags {
		if semver.Compare(targetVersion, *tag.Name) < 0 {
			targetVersion = *tag.Name
		}
	}

	if targetVersion == info.Main.Version {
		fmt.Printf("%s\tup to date: %s\n", info.Main.Path, info.Main.Version)
		return nil
	}

	// TODO: env?

	args := []string{"go", "install"}
	for _, setting := range info.Settings {
		if in(setting.Key, preserveSettings) {
			args = append(args, fmt.Sprintf("%s=%s", setting.Key, setting.Value))
		}
	}
	args = append(args, fmt.Sprintf("%s@%s", info.Path, targetVersion))

	b := strings.Builder{}
	b.WriteString(info.Main.Path)
	b.WriteRune('\t')
	b.WriteString(info.Main.Version)
	b.WriteString(" -> ")
	b.WriteString(targetVersion)
	b.WriteRune('\t')
	b.WriteString(strings.Join(args, " "))

	fmt.Println(b.String())
	return nil
}

func goBin() (string, error) {
	bin := os.Getenv("GOBIN")
	if bin != "" {
		return bin, nil
	}

	gopath := os.Getenv("GOPATH")
	if gopath != "" {
		return filepath.Join(gopath, "bin"), nil
	}

	home := os.Getenv("HOME")
	if home != "" {
		return filepath.Join(home, "go", "bin"), nil
	}

	return "", fmt.Errorf("go bin: unable to determine GOBIN: $GOBIN, $GOPATH and $HOME are not set")
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
