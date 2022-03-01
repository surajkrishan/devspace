package engine

import (
	"context"
	"fmt"
	devspacecontext "github.com/loft-sh/devspace/pkg/devspace/context"
	enginecommands "github.com/loft-sh/devspace/pkg/devspace/pipeline/engine/commands"
	"github.com/loft-sh/devspace/pkg/devspace/pipeline/types"
	"github.com/loft-sh/devspace/pkg/util/downloader"
	"github.com/loft-sh/devspace/pkg/util/downloader/commands"
	"github.com/loft-sh/devspace/pkg/util/log"
	"github.com/pkg/errors"
	"io"
	"mvdan.cc/sh/v3/interp"
	"os"
	"time"
)

type ExecHandler interface {
	ExecHandler(ctx context.Context, args []string) error
}

func NewExecHandler(ctx *devspacecontext.Context, stdout io.Writer, pipeline types.Pipeline, enablePipelineCommands bool) ExecHandler {
	return &execHandler{
		ctx:                    ctx,
		stdout:                 stdout,
		pipeline:               pipeline,
		enablePipelineCommands: enablePipelineCommands,
	}
}

type execHandler struct {
	ctx                    *devspacecontext.Context
	stdout                 io.Writer
	pipeline               types.Pipeline
	enablePipelineCommands bool
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
		WithWorkingDir(hc.Dir)
	if e.stdout != nil && e.stdout == hc.Stdout {
		devCtx = devCtx.WithLogger(e.ctx.Log)
	} else {
		devCtx = devCtx.WithLogger(log.NewStreamLogger(hc.Stdout, e.ctx.Log.GetLevel()))
	}

	switch command {
	case "run_pipelines":
		return e.executePipelineCommand(ctx, command, func() error {
			return enginecommands.Pipeline(devCtx, e.pipeline, args)
		})
	case "build_images":
		return e.executePipelineCommand(ctx, command, func() error {
			return enginecommands.Build(devCtx, args)
		})
	case "create_deployments":
		return e.executePipelineCommand(ctx, command, func() error {
			return enginecommands.Deploy(devCtx, args)
		})
	case "start_dev":
		return e.executePipelineCommand(ctx, command, func() error {
			return enginecommands.StartDev(devCtx, e.pipeline.DevPodManager(), args)
		})
	case "stop_dev":
		return e.executePipelineCommand(ctx, command, func() error {
			return enginecommands.StopDev(devCtx, e.pipeline.DevPodManager(), args)
		})
	case "run_dependencies_pipeline":
		return e.executePipelineCommand(ctx, command, func() error {
			return enginecommands.Dependency(devCtx, e.pipeline.DependencyRegistry(), args)
		})
	}

	return false, nil
}

func (e *execHandler) executePipelineCommand(ctx context.Context, command string, commandFn func() error) (bool, error) {
	if !e.enablePipelineCommands {
		hc := interp.HandlerCtx(ctx)
		_, _ = fmt.Fprintln(hc.Stderr, fmt.Errorf("%s: cannot execute the command because it can only be executed within a pipeline step", command))
		return true, interp.NewExitStatus(1)
	}

	return true, handleError(ctx, command, commandFn())
}

func handleError(ctx context.Context, command string, err error) error {
	if err == nil {
		return interp.NewExitStatus(0)
	}

	hc := interp.HandlerCtx(ctx)
	_, _ = fmt.Fprintln(hc.Stderr, errors.Wrap(err, command))
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
	}
	return nil
}