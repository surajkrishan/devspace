package kubectl

import (
	"github.com/loft-sh/devspace/pkg/devspace/config/loader/variable/legacy"
	devspacecontext "github.com/loft-sh/devspace/pkg/devspace/context"
	"io"
	"strings"

	"github.com/loft-sh/devspace/pkg/util/downloader"
	"github.com/loft-sh/devspace/pkg/util/downloader/commands"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/ghodss/yaml"
	"github.com/pkg/errors"

	"github.com/loft-sh/devspace/pkg/devspace/config/versions/latest"
	"github.com/loft-sh/devspace/pkg/devspace/deploy/deployer"
	"github.com/loft-sh/devspace/pkg/util/hash"
)

// DeployConfig holds the necessary information for kubectl deployment
type DeployConfig struct {
	Name        string
	CmdPath     string
	Context     string
	Namespace   string
	IsInCluster bool
	Manifests   []string

	DeploymentConfig *latest.DeploymentConfig
	commandExecuter  commandExecuter
}

// New creates a new deploy config for kubectl
func New(ctx *devspacecontext.Context, deployConfig *latest.DeploymentConfig) (deployer.Interface, error) {
	if deployConfig.Kubectl == nil {
		return nil, errors.New("error creating kubectl deploy config: kubectl is nil")
	} else if deployConfig.Kubectl.Manifests == nil {
		return nil, errors.New("no manifests defined for kubectl deploy")
	}

	// make sure kubectl exists
	var (
		err      error
		executer = &executer{}
		cmdPath  string
	)
	if deployConfig.Kubectl.CmdPath != "" {
		cmdPath = deployConfig.Kubectl.CmdPath
	} else {
		cmdPath, err = downloader.NewDownloader(commands.NewKubectlCommand(), ctx.Log).EnsureCommand()
		if err != nil {
			return nil, err
		}
	}

	manifests := []string{}
	for _, ptrManifest := range deployConfig.Kubectl.Manifests {
		manifest := strings.Replace(ptrManifest, "*", "", -1)
		if deployConfig.Kubectl.Kustomize != nil && *deployConfig.Kubectl.Kustomize {
			manifest = strings.TrimSuffix(manifest, "kustomization.yaml")
		}

		manifests = append(manifests, manifest)
	}

	if ctx.KubeClient == nil {
		return &DeployConfig{
			Name:      deployConfig.Name,
			CmdPath:   cmdPath,
			Manifests: manifests,

			DeploymentConfig: deployConfig,
			commandExecuter:  executer,
		}, nil
	}

	namespace := ctx.KubeClient.Namespace()
	if deployConfig.Namespace != "" {
		namespace = deployConfig.Namespace
	}

	return &DeployConfig{
		Name:        deployConfig.Name,
		CmdPath:     cmdPath,
		Context:     ctx.KubeClient.CurrentContext(),
		Namespace:   namespace,
		Manifests:   manifests,
		IsInCluster: ctx.KubeClient.IsInCluster(),

		DeploymentConfig: deployConfig,
		commandExecuter:  executer,
	}, nil
}

// Render writes the generated manifests to the out stream
func (d *DeployConfig) Render(ctx *devspacecontext.Context, out io.Writer) error {
	for _, manifest := range d.Manifests {
		_, replacedManifest, err := d.getReplacedManifest(ctx, manifest)
		if err != nil {
			return errors.Errorf("%v\nPlease make sure `kubectl apply` does work locally with manifest `%s`", err, manifest)
		}

		_, _ = out.Write([]byte(replacedManifest))
		_, _ = out.Write([]byte("\n---\n"))
	}

	return nil
}

// Status prints the status of all matched manifests from kubernetes
func (d *DeployConfig) Status(ctx *devspacecontext.Context) (*deployer.StatusResult, error) {
	// TODO: parse kubectl get output into the required string array
	manifests := strings.Join(d.Manifests, ",")
	if len(manifests) > 20 {
		manifests = manifests[:20] + "..."
	}

	return &deployer.StatusResult{
		Name:   d.Name,
		Type:   "Manifests",
		Target: manifests,
		Status: "N/A",
	}, nil
}

// Delete deletes all matched manifests from kubernetes
func (d *DeployConfig) Delete(ctx *devspacecontext.Context) error {
	ctx.Log.StartWait("Deleting manifests with kubectl")
	defer ctx.Log.StopWait()

	for i := len(d.Manifests) - 1; i >= 0; i-- {
		manifest := d.Manifests[i]
		_, replacedManifest, err := d.getReplacedManifest(ctx, manifest)
		if err != nil {
			return err
		}

		args := d.getCmdArgs("delete", "--ignore-not-found=true")
		args = append(args, d.DeploymentConfig.Kubectl.DeleteArgs...)

		stringReader := strings.NewReader(replacedManifest)
		cmd := d.commandExecuter.GetCommand(d.CmdPath, args)
		err = cmd.Run(ctx.WorkingDir, ctx.Log, ctx.Log, stringReader)
		if err != nil {
			return err
		}
	}

	ctx.Config.RemoteCache().DeleteDeploymentCache(d.DeploymentConfig.Name)
	return nil
}

// Deploy deploys all specified manifests via kubectl apply and adds to the specified image names the corresponding tags
func (d *DeployConfig) Deploy(ctx *devspacecontext.Context, _ bool) (bool, error) {
	deployCache, _ := ctx.Config.RemoteCache().GetDeploymentCache(d.DeploymentConfig.Name)

	// Hash the manifests
	manifestsHash := ""
	for _, manifest := range d.Manifests {
		if strings.HasPrefix(manifest, "http://") || strings.HasPrefix(manifest, "https://") {
			manifestsHash += hash.String(manifest)
			continue
		}

		// Check if the chart directory has changed
		manifest = ctx.ResolvePath(manifest)
		hash, err := hash.Directory(manifest)
		if err != nil {
			return false, errors.Errorf("Error hashing %s: %v", manifest, err)
		}

		manifestsHash += hash
	}

	// Hash the deployment config
	configStr, err := yaml.Marshal(d.DeploymentConfig)
	if err != nil {
		return false, errors.Wrap(err, "marshal deployment config")
	}

	deploymentConfigHash := hash.String(string(configStr))

	// We force the redeploy of kubectl deployments for now, because we don't know if they are already currently deployed or not,
	// so it is better to force deploy them, which usually takes almost no time and is better than taking the risk of skipping a needed deployment
	// forceDeploy = forceDeploy || deployCache.KubectlManifestsHash != manifestsHash || deployCache.DeploymentConfigHash != deploymentConfigHash
	forceDeploy := true

	ctx.Log.StartWait("Applying manifests with kubectl")
	defer ctx.Log.StopWait()

	wasDeployed := false

	for _, manifest := range d.Manifests {
		shouldRedeploy, replacedManifest, err := d.getReplacedManifest(ctx, manifest)
		if err != nil {
			return false, errors.Errorf("%v\nPlease make sure `kubectl apply` does work locally with manifest `%s`", err, manifest)
		}

		if shouldRedeploy || forceDeploy {
			stringReader := strings.NewReader(replacedManifest)
			args := d.getCmdArgs("apply", "--force")
			args = append(args, d.DeploymentConfig.Kubectl.ApplyArgs...)

			cmd := d.commandExecuter.GetCommand(d.CmdPath, args)
			err = cmd.Run(ctx.WorkingDir, ctx.Log, ctx.Log, stringReader)
			if err != nil {
				return false, errors.Errorf("%v\nPlease make sure the command `kubectl apply` does work locally with manifest `%s`", err, manifest)
			}

			wasDeployed = true
		} else {
			ctx.Log.Infof("Skipping manifest %s", manifest)
		}
	}

	deployCache.KubectlManifestsHash = manifestsHash
	deployCache.DeploymentConfigHash = deploymentConfigHash
	ctx.Config.RemoteCache().SetDeploymentCache(d.DeploymentConfig.Name, deployCache)
	return wasDeployed, nil
}

func (d *DeployConfig) getReplacedManifest(ctx *devspacecontext.Context, manifest string) (bool, string, error) {
	objects, err := d.buildManifests(ctx, manifest)
	if err != nil {
		return false, "", err
	}

	// Split output into the yamls
	var (
		replaceManifests = []string{}
		shouldRedeploy   = false
	)

	for _, resource := range objects {
		if resource.Object == nil {
			continue
		}

		if d.DeploymentConfig.Kubectl.ReplaceImageTags {
			redeploy, err := legacy.ReplaceImageNamesStringMap(resource.Object, ctx.Config, ctx.Dependencies, map[string]bool{"image": true})
			if err != nil {
				return false, "", err
			} else if redeploy {
				shouldRedeploy = true
			}
		}

		replacedManifest, err := yaml.Marshal(resource)
		if err != nil {
			return false, "", errors.Wrap(err, "marshal yaml")
		}

		replaceManifests = append(replaceManifests, string(replacedManifest))
	}

	return shouldRedeploy, strings.Join(replaceManifests, "\n---\n"), nil
}

func (d *DeployConfig) getCmdArgs(method string, additionalArgs ...string) []string {
	args := []string{}
	if d.Context != "" && !d.IsInCluster {
		args = append(args, "--context", d.Context)
	}
	if d.Namespace != "" {
		args = append(args, "--namespace", d.Namespace)
	}

	args = append(args, method)
	if additionalArgs != nil {
		args = append(args, additionalArgs...)
	}

	args = append(args, "-f", "-")
	return args
}

func (d *DeployConfig) buildManifests(ctx *devspacecontext.Context, manifest string) ([]*unstructured.Unstructured, error) {
	// Check if we should use kustomize or kubectl
	if d.DeploymentConfig.Kubectl.Kustomize != nil && *d.DeploymentConfig.Kubectl.Kustomize && d.isKustomizeInstalled(ctx.WorkingDir, "kustomize") {
		return NewKustomizeBuilder("kustomize", d.DeploymentConfig, ctx.Log).Build(ctx.WorkingDir, manifest, d.commandExecuter.RunCommand)
	}

	// Build with kubectl
	return NewKubectlBuilder(d.CmdPath, d.DeploymentConfig, d.Context, d.Namespace, d.IsInCluster).Build(ctx.WorkingDir, manifest, d.commandExecuter.RunCommand)
}

func (d *DeployConfig) isKustomizeInstalled(dir, path string) bool {
	_, err := d.commandExecuter.RunCommand(dir, path, []string{"version"})
	return err == nil
}
