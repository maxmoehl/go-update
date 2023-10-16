//go:build unix

package main

import (
	"debug/buildinfo"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const (
	goBinEnv        = "GOBIN"
	goPathEnv       = "GOPATH"
	goProxyEnv      = "GOPROXY"
	goMinVersionEnv = "GOMINVERSION"
	homeEnv         = "HOME"

	logErrorKey = "error"
)

var (
	client = http.DefaultClient

	logLevel = &slog.LevelVar{}

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

type usageError error

func init() {
	var err error
	defer func() {
		if err != nil {
			fmt.Printf("error: init: %s\n", err.Error())
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
	slog.SetDefault(slog.New(newConsoleHandler(os.Stderr, logLevel)))

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

func main() {
	err := Main()
	if err != nil {
		var usageErr usageError
		if errors.As(err, &usageErr) {
			fmt.Printf("Usage: %s [ update (default) | list ]\n", os.Args[0])
		}

		fmt.Printf("error: main: %s\n", err.Error())
		os.Exit(2) // exit code 2: generic error during execution
	}
}

func Main() error {
	var list bool
	if len(os.Args) > 2 {
		return usageError(fmt.Errorf("only one argument can be provided"))
	} else if len(os.Args) > 1 {
		switch os.Args[1] {
		case "update": // default, no-op
		case "list":
			list = true
		default:
			return usageError(fmt.Errorf("unknown command '%s'", os.Args[1]))
		}
	}

	entries, err := fs.ReadDir(os.DirFS(goBin), ".")
	if err != nil {
		return err
	}

	var artefacts []Artefact

	for _, entry := range entries {
		executablePath := filepath.Join(goBin, entry.Name())
		log := slog.With("path", executablePath)

		if entry.IsDir() {
			log.Info("skipping directory", "name", entry.Name())
			continue
		}

		fileInfo, err := entry.Info()
		if err != nil {
			log.Error("reading file info failed", logErrorKey, err)
			continue
		}

		if !executable(fileInfo.Mode()) {
			log.Info("skipping non-executable file")
			continue
		}
		if !fileInfo.Mode().Type().IsRegular() {
			log.Info("skipping non-regular file")
			continue
		}

		info, err := buildinfo.ReadFile(executablePath)
		if err != nil {
			log.Error("reading build info failed", logErrorKey, err)
			continue
		}
		if info.GoVersion < minGoVersion {
			log.Error("go version too old to update", "go-version", info.GoVersion)
			continue
		}

		a, err := NewArtefact(info)
		if err != nil {
			log.Error("loading artefact failed", logErrorKey, err)
			continue
		}
		artefacts = append(artefacts, a)

		log.Info("loaded artefact", "installed-version", a.InstalledVersion(),
			"target-version", a.TargetVersion())

		if list || !a.NeedsUpdate() {
			continue
		}

		err = a.Update()
		if err != nil {
			log.Error("installing target version failed", logErrorKey, err)
			continue
		}

		log.Info("updated artefact")
	}

	if list {
		printArtefacts(artefacts)
	}

	return nil
}

func install(pkg string, version string) error {
	args := []string{"go", "install"}
	if logLevel.Level() <= slog.LevelInfo {
		args = append(args, "-v")
	}
	if logLevel.Level() <= slog.LevelDebug {
		args = append(args, "-x")
	}
	args = append(args, fmt.Sprintf("%s@%s", pkg, version))
	cmd := exec.Cmd{
		Path:   goCli,
		Args:   args,
		Stdout: os.Stdout,
		Stderr: os.Stderr,
	}

	slog.Debug("executing command", "cmd", cmd.String())

	return cmd.Run()
}

func executable(mode os.FileMode) bool {
	return mode&0111 != 0
}

func printArtefacts(artefacts []Artefact) {
	var table [][]string
	table = append(table, []string{"Program", "Installed Version", "Latest Version"})
	for _, a := range artefacts {
		table = append(table, []string{
			a.InstallPath(),
			a.InstalledVersion(),
			a.TargetVersion(),
		})
	}

	tablePrint(table)
}

func tablePrint(table [][]string) {
	var columnWidth []int

	for _, row := range table {
		for ci, column := range row {
			if ci >= len(columnWidth) {
				// new column
				// TODO: this is not the correct way to get the width of a string
				columnWidth = append(columnWidth, len(column))
			} else if len(column) > columnWidth[ci] {
				// new biggest value for column
				columnWidth[ci] = len(column)
			}
		}
	}

	spaces := func(n int) string {
		b := strings.Builder{}
		for i := 0; i < n; i++ {
			b.WriteRune(' ')
		}
		return b.String()
	}

	b := strings.Builder{}
	for _, row := range table {
		for ci, column := range row {
			b.WriteString(column)
			if ci == len(row)-1 {
				continue
			}
			b.WriteString(spaces(columnWidth[ci] - len(column) + 1))
		}
		b.WriteRune('\n')
	}

	fmt.Print(b.String())
}
