package internal

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"os/exec"
)

var goBin string

func init() {
	var err error
	goBin, err = exec.LookPath("go")
	if err != nil {
		panic(fmt.Sprintf("unable to locate go binary: %s", err.Error()))
	}
}

func goCmd(args []string, v any) error {
	errBuf := &bytes.Buffer{}
	outBuf := &bytes.Buffer{}
	c := exec.Cmd{
		Path:   goBin,
		Args:   append([]string{"go"}, args...),
		Stdout: outBuf,
		Stderr: errBuf,
	}

	slog.Debug("executing command", "cmd", c.String())

	err := c.Run()
	if err != nil {
		return fmt.Errorf("%w: %s", err, errBuf.String())
	}

	if v == nil {
		// We might not care about the result.
		return nil
	}

	return json.Unmarshal(outBuf.Bytes(), v)
}

type moduleVersions struct {
	Versions []string
}

func ListVersions(module string) ([]string, error) {
	var v moduleVersions

	err := goCmd([]string{"list", "-versions", "-json", "-m", module}, &v)
	if err != nil {
		return nil, fmt.Errorf("go list: %w", err)
	}

	return v.Versions, nil
}

func Install(pkg string, version string) error {
	return goCmd([]string{"install", fmt.Sprintf("%s@%s", pkg, version)}, nil)
}
