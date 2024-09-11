package internal

import (
	"log/slog"
	"os/exec"
)

func AttrErr(err error) slog.Attr {
	return slog.String("error", err.Error())
}

func AttrCmd(cmd exec.Cmd) slog.Attr {
	return slog.String("cmd", cmd.String())
}
