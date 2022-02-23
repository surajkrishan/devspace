package engine

import (
	"context"
	"fmt"
	devspacecontext "github.com/loft-sh/devspace/pkg/devspace/context"
	"github.com/loft-sh/devspace/pkg/devspace/devpod"
	enginecommands "github.com/loft-sh/devspace/pkg/devspace/pipeline/engine/commands"
	"github.com/loft-sh/devspace/pkg/util/downloader"
	"github.com/loft-sh/devspace/pkg/util/downloader/commands"
	"github.com/loft-sh/devspace/pkg/util/log"
	"mvdan.cc/sh/v3/interp"
	"os"
	"time"
)

type ExecHandler interface {
	ExecHandler(ctx context.Context, args []string) error
}

func NewExecHandler(ctx *devspacecontext.Context, manager devpod.Manager) ExecHandler {
	return &execHandler{
		ctx:     ctx,
		manager: manager,
	}
}

type execHandler struct {
	ctx     *devspacecontext.Context
	manager devpod.Manager
}

func (e *execHandler) ExecHandler(ctx context.Context, args []string) error {
	if len(args) > 0 {
		// handle special pipeline commands
		handled, err := e.handlePipelineCommands(ctx, args[0], args[1:])
		if handled || err != nil {
			return err
		}

		// handle some special commands that are not found locally
		hc := interp.HandlerCtx(ctx)
		_, err = lookPathDir(hc.Dir, hc.Env, args[0])
		if err != nil {
			err = e.fallbackCommands(ctx, args[0], args[1:])
			if err != nil {
				return err
			}
		}
	}

	return interp.DefaultExecHandler(2*time.Second)(ctx, args)
}

func (e *execHandler) handlePipelineCommands(ctx context.Context, command string, args []string) (bool, error) {
	hc := interp.HandlerCtx(ctx)
	devCtx := e.ctx.WithContext(ctx).
		WithWorkingDir(hc.Dir).
		WithLogger(log.NewStreamLogger(hc.Stdout, e.ctx.Log.GetLevel()))

	switch command {
	case "build":
		err := enginecommands.Build(devCtx, args)
		return true, handleError(ctx, err)
	case "deploy":
		err := enginecommands.Deploy(devCtx, args)
		return true, handleError(ctx, err)
	case "dev":
		err := enginecommands.Dev(devCtx, e.manager, args)
		return true, handleError(ctx, err)
	}

	return false, nil
}

func handleError(ctx context.Context, err error) error {
	if err == nil {
		return interp.NewExitStatus(0)
	}

	hc := interp.HandlerCtx(ctx)
	_, _ = fmt.Fprintln(hc.Stderr, err)
	return interp.NewExitStatus(1)
}

func (e *execHandler) fallbackCommands(ctx context.Context, command string, args []string) error {
	logger := log.GetFileLogger("shell")
	hc := interp.HandlerCtx(ctx)

	switch command {
	case "cat":
		err := enginecommands.Cat(&hc, args)
		if err != nil {
			_, _ = fmt.Fprintln(hc.Stderr, err)
			return interp.NewExitStatus(1)
		}
		return interp.NewExitStatus(0)
	case "kubectl":
		path, err := downloader.NewDownloader(commands.NewKubectlCommand(), logger).EnsureCommand()
		if err != nil {
			_, _ = fmt.Fprintln(hc.Stderr, err)
			return interp.NewExitStatus(127)
		}
		command = path
	case "helm":
		path, err := downloader.NewDownloader(commands.NewHelmV3Command(), logger).EnsureCommand()
		if err != nil {
			_, _ = fmt.Fprintln(hc.Stderr, err)
			return interp.NewExitStatus(127)
		}
		command = path
	case "devspace":
		bin, err := os.Executable()
		if err != nil {
			_, _ = fmt.Fprintln(hc.Stderr, err)
			return interp.NewExitStatus(1)
		}
		command = bin
	default:
		_, _ = fmt.Fprintln(hc.Stderr, "command is not found.")
		return interp.NewExitStatus(127)
	}
	return nil
}
