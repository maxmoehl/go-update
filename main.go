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

	"moehl.dev/go-update/internal"
)

const (
	goBinEnv        = "GOBIN"
	goPathEnv       = "GOPATH"
	goMinVersionEnv = "GOMINVERSION"
	homeEnv         = "HOME"

	ignorePath = ".goupdateignore"
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

	excludePatterns []string
	includePatterns []string
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
		logLevel.Set(slog.LevelInfo)
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

	goCli, err = exec.LookPath("go")
	if err != nil {
		err = fmt.Errorf("looking up go cli path: %w", err)
		return
	}

	excludePatterns, includePatterns, err = ignoreFile(filepath.Join(goBin, ignorePath))
	if err != nil {
		err = fmt.Errorf("load ignore file: %w", err)
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

		if ignore(excludePatterns, includePatterns, entry.Name()) {
			log.Debug("ignoring file")
			continue
		}

		if entry.IsDir() {
			log.Debug("skipping directory", "name", entry.Name())
			continue
		}

		fileInfo, err := entry.Info()
		if err != nil {
			log.Error("reading file info failed", internal.AttrErr(err))
			continue
		}

		if !executable(fileInfo.Mode()) {
			log.Debug("skipping non-executable file")
			continue
		}
		if !fileInfo.Mode().Type().IsRegular() {
			log.Debug("skipping non-regular file")
			continue
		}

		execFile, err := os.Open(executablePath)
		if err != nil {
			log.Error("unable to open executable", internal.AttrErr(err))
			continue
		}

		magic := make([]byte, 2)
		_, err = execFile.ReadAt(magic, 0)
		if err != nil {
			log.Error("unable to read magic bytes from executable", internal.AttrErr(err))
			continue
		}

		if string(magic) == "#!" {
			log.Debug("skipping shell script with shebang")
			continue
		}

		info, err := buildinfo.Read(execFile)
		if err != nil {
			log.Error("reading build info failed", internal.AttrErr(err))
			continue
		}
		if info.GoVersion < minGoVersion {
			log.Error("go version too old to update", "go-version", info.GoVersion)
			continue
		}

		a, err := NewArtefact(info)
		if err != nil {
			log.Error("loading artefact failed", internal.AttrErr(err))
			continue
		}
		artefacts = append(artefacts, a)

		log.Debug("loaded artefact",
			"installed-version", a.InstalledVersion(),
			"target-version", a.TargetVersion())

		if list || !a.NeedsUpdate() {
			continue
		}

		err = a.Update()
		if err != nil {
			log.Error("installing target version failed", internal.AttrErr(err))
			continue
		}

		log.Info("updated artefact",
			"installed-version", a.InstalledVersion(),
			"target-version", a.TargetVersion())
	}

	if list {
		printArtefacts(artefacts)
	}

	return nil
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
