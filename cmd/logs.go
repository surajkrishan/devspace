package cmd

import (
	"context"
	"fmt"
	runtimevar "github.com/loft-sh/devspace/pkg/devspace/config/loader/variable/runtime"
	devspacecontext "github.com/loft-sh/devspace/pkg/devspace/context"
	"github.com/loft-sh/devspace/pkg/devspace/imageselector"
	"github.com/loft-sh/devspace/pkg/devspace/kubectl/selector"
	"github.com/loft-sh/devspace/pkg/devspace/services/logs"
	"os"
	"time"

	"github.com/loft-sh/devspace/cmd/flags"
	config2 "github.com/loft-sh/devspace/pkg/devspace/config"
	"github.com/loft-sh/devspace/pkg/devspace/config/loader"
	"github.com/loft-sh/devspace/pkg/devspace/dependency"
	"github.com/loft-sh/devspace/pkg/devspace/dependency/types"
	"github.com/loft-sh/devspace/pkg/devspace/hook"
	"github.com/loft-sh/devspace/pkg/devspace/kubectl"
	"github.com/loft-sh/devspace/pkg/devspace/plugin"
	"github.com/loft-sh/devspace/pkg/devspace/services/targetselector"
	"github.com/loft-sh/devspace/pkg/util/factory"
	"github.com/loft-sh/devspace/pkg/util/log"
	"github.com/loft-sh/devspace/pkg/util/message"
	"github.com/pkg/errors"
	"github.com/spf13/cobra"
)

// LogsCmd holds the logs cmd flags
type LogsCmd struct {
	*flags.GlobalFlags

	LabelSelector string
	ImageSelector string
	Image         string
	Container     string
	Pod           string
	Pick          bool

	Follow            bool
	Wait              bool
	LastAmountOfLines int
}

// NewLogsCmd creates a new login command
func NewLogsCmd(f factory.Factory, globalFlags *flags.GlobalFlags) *cobra.Command {
	cmd := &LogsCmd{GlobalFlags: globalFlags}

	logsCmd := &cobra.Command{
		Use:   "logs",
		Short: "Prints the logs of a pod and attaches to it",
		Long: `
#######################################################
#################### devspace logs ####################
#######################################################
Logs prints the last log of a pod container and attachs 
to it

Example:
devspace logs
devspace logs --namespace=mynamespace
#######################################################
	`,
		Args: cobra.NoArgs,
		RunE: func(cobraCmd *cobra.Command, args []string) error {
			plugin.SetPluginCommand(cobraCmd, args)
			return cmd.RunLogs(f)
		},
	}

	logsCmd.Flags().StringVarP(&cmd.Container, "container", "c", "", "Container name within pod where to execute command")
	logsCmd.Flags().StringVar(&cmd.Pod, "pod", "", "Pod to print the logs of")
	logsCmd.Flags().StringVarP(&cmd.LabelSelector, "label-selector", "l", "", "Comma separated key=value selector list (e.g. release=test)")
	logsCmd.Flags().StringVar(&cmd.ImageSelector, "image-selector", "", "The image to search a pod for (e.g. nginx, nginx:latest, ${runtime.images.app}, nginx:${runtime.images.app.tag})")
	logsCmd.Flags().StringVar(&cmd.Image, "image", "", "Image is the config name of an image to select in the devspace config (e.g. 'default'), it is NOT a docker image like myuser/myimage")
	logsCmd.Flags().BoolVar(&cmd.Pick, "pick", true, "Select a pod")
	logsCmd.Flags().BoolVarP(&cmd.Follow, "follow", "f", false, "Attach to logs afterwards")
	logsCmd.Flags().IntVar(&cmd.LastAmountOfLines, "lines", 200, "Max amount of lines to print from the last log")
	logsCmd.Flags().BoolVar(&cmd.Wait, "wait", false, "Wait for the pod(s) to start if they are not running")

	return logsCmd
}

// RunLogs executes the functionality devspace logs
func (cmd *LogsCmd) RunLogs(f factory.Factory) error {
	// Set config root
	log := f.GetLog()
	configOptions := cmd.ToConfigOptions()
	configLoader := f.NewConfigLoader(cmd.ConfigPath)
	_, err := configLoader.SetDevSpaceRoot(log)
	if err != nil {
		return err
	}

	// Get kubectl client
	client, err := f.NewKubeClientFromContext(cmd.KubeContext, cmd.Namespace)
	if err != nil {
		return errors.Wrap(err, "create kube client")
	}

	// create the context
	ctx := &devspacecontext.Context{
		Context:    context.Background(),
		KubeClient: client,
		Log:        log,
	}

	// Execute plugin hook
	err = hook.ExecuteHooks(ctx, nil, "logs")
	if err != nil {
		return err
	}

	// get image selector if specified
	imageSelector, err := getImageSelector(client, configLoader, configOptions, cmd.Image, cmd.ImageSelector, log)
	if err != nil {
		return err
	}

	// Build options
	options := targetselector.NewOptionsFromFlags(cmd.Container, cmd.LabelSelector, imageSelector, cmd.Namespace, cmd.Pod).
		WithPick(cmd.Pick).
		WithWait(cmd.Wait).
		WithContainerFilter(selector.FilterTerminatingContainers)
	if cmd.Wait {
		options = options.WithWaitingStrategy(targetselector.NewUntilNotWaitingStrategy(time.Second * 2))
	}

	// Start terminal
	err = logs.StartLogsWithWriter(ctx, targetselector.NewTargetSelector(options), cmd.Follow, int64(cmd.LastAmountOfLines), os.Stdout)
	if err != nil {
		return err
	}

	return nil
}

func getImageSelector(client kubectl.Client, configLoader loader.ConfigLoader, configOptions *loader.ConfigOptions, image, imageSelector string, log log.Logger) ([]string, error) {
	var imageSelectors []string
	if imageSelector != "" {
		var (
			err          error
			config       config2.Config
			dependencies []types.Dependency
		)
		if !configLoader.Exists() {
			config = config2.Ensure(nil)
		} else {
			config, err = configLoader.Load(client, configOptions, log)
			if err != nil {
				return nil, err
			}

			dependencies, err = dependency.NewManager(config, client, configOptions, log).ResolveAll(dependency.ResolveOptions{
				Silent: true,
			})
			if err != nil {
				log.Warnf("Error resolving dependencies: %v", err)
			}
		}

		resolved, err := runtimevar.NewRuntimeResolver(".", true).FillRuntimeVariablesAsImageSelector(imageSelector, config, dependencies)
		if err != nil {
			return nil, err
		}

		imageSelectors = append(imageSelectors, resolved.Image)
	} else if image != "" {
		log.Warnf("Flag --image is deprecated, please use --image-selector instead")
		if !configLoader.Exists() {
			return nil, errors.New(message.ConfigNotFound)
		}

		config, err := configLoader.Load(client, configOptions, log)
		if err != nil {
			return nil, err
		}

		resolved, err := dependency.NewManager(config, client, configOptions, log).ResolveAll(dependency.ResolveOptions{
			Silent: true,
		})
		if err != nil {
			log.Warnf("Error resolving dependencies: %v", err)
		}

		imageSelector, err := imageselector.Resolve(image, config, resolved)
		if err != nil {
			return nil, err
		} else if imageSelector == nil {
			return nil, fmt.Errorf("couldn't find an image with name %s in devspace config", image)
		}

		imageSelectors = append(imageSelectors, imageSelector.Image)
	}

	return imageSelectors, nil
}
