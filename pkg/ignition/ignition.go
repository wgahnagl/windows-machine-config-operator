package ignition

import (
	"context"
	"fmt"
	"sort"
	"strings"

	ignCfg "github.com/coreos/ignition/v2/config/v3_5"
	ignCfgTypes "github.com/coreos/ignition/v2/config/v3_5/types"
	mcfg "github.com/openshift/api/machineconfiguration/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Watch permission are needed in order to populate the cache. We use a cached client to list machineconfig and
// controllerconfig resources.
//+kubebuilder:rbac:groups="machineconfiguration.openshift.io",resources=machineconfigs,verbs=list;watch
//+kubebuilder:rbac:groups="machineconfiguration.openshift.io",resources=controllerconfigs,verbs=list;watch

const (
	// kubeletSystemdName is the name of the systemd service that the kubelet runs under,
	// this is used to parse the kubelet args
	kubeletSystemdName = "kubelet.service"
	// CloudConfigOption is the kubelet CLI option for the cloud configuration file
	CloudConfigOption = "cloud-config"
	// CloudProviderOption is the kubelet CLI option for cloud provider
	CloudProviderOption = "cloud-provider"
	// RenderedWorkerPrefix allows identification of the rendered worker MachineConfig, the combination of all worker
	// MachineConfigs.
	RenderedWorkerPrefix = "rendered-worker-"
	// CloudConfigPath is the path to the cloud config file as defined in ignition
	CloudConfigPath = "/etc/kubernetes/cloud.conf"
	// CloudConfigPath is the path to the ecr credential provider config as defined in ignition
	ECRCredentialProviderPath = "/etc/kubernetes/credential-providers/ecr-credential-provider.yaml"
)

// Ignition is a representation of an Ignition resource
type Ignition struct {
	config        ignCfgTypes.Config
	kubeletCAData []byte
}

// New returns a new instance of Ignition
func New(ctx context.Context, c client.Client) (*Ignition, error) {
	log := ctrl.Log.WithName("ignition")
	machineConfigs := &mcfg.MachineConfigList{}
	err := c.List(ctx, machineConfigs)
	if err != nil {
		return nil, err
	}
	renderedWorker, err := getLatestRenderedWorker(machineConfigs.Items)
	if err != nil {
		return nil, err
	}

	configuration, report, err := ignCfg.ParseCompatibleVersion(renderedWorker.Spec.Config.Raw)
	if err != nil || report.IsFatal() {
		return nil, fmt.Errorf("failed to parse MachineConfig ignition: %v\nReport: %v", err, report)
	}
	ign := &Ignition{
		config: configuration,
	}
	log.V(1).Info("parsed", "machineconfig", renderedWorker.GetName(), "using ignition version",
		configuration.Ignition.Version)

	ccList := mcfg.ControllerConfigList{}
	if err := c.List(ctx, &ccList); err != nil {
		return nil, err
	}
	var kubeletCAData []byte
	for _, item := range ccList.Items {
		if item.Spec.KubeAPIServerServingCAData != nil {
			log.V(1).Info("processing kubelet-ca", "ControllerConfig", item.Name)
			kubeletCAData = item.Spec.KubeAPIServerServingCAData
			break
		}
	}
	if len(kubeletCAData) == 0 {
		return nil, fmt.Errorf("cannot find kubelet-ca")
	}
	// set kubelet-ca raw data
	ign.kubeletCAData = kubeletCAData

	return ign, nil
}

// GetKubeletCAData is a getter for kubelet CA raw data
func (ign *Ignition) GetKubeletCAData() []byte {
	return ign.kubeletCAData
}

// GetFiles is a getter for the files embedded within the ignition spec
func (ign *Ignition) GetFiles() []ignCfgTypes.File {
	return ign.config.Storage.Files
}

// GetKubeletArgs returns a set of arguments for kubelet.exe, as specified in the ignition file
func (ign *Ignition) GetKubeletArgs() (map[string]string, error) {
	var kubeletUnit ignCfgTypes.Unit
	for _, unit := range ign.config.Systemd.Units {
		if unit.Name == kubeletSystemdName {
			kubeletUnit = unit
			break
		}
	}
	if kubeletUnit.Contents == nil {
		return nil, fmt.Errorf("ignition missing kubelet systemd unit file")
	}
	argsFromIgnition, err := parseKubeletArgs(*kubeletUnit.Contents)
	if err != nil {
		return nil, fmt.Errorf("error parsing kubelet systemd unit args: %w", err)
	}
	return argsFromIgnition, nil
}

// getLatestRenderedWorker returns the most recently created rendered worker MachineConfig
func getLatestRenderedWorker(machineConfigs []mcfg.MachineConfig) (*mcfg.MachineConfig, error) {
	// Grab the latest rendered-worker MachineConfig by sorting the MachineConfig list by the latest creation
	// timestamp first.
	sort.Slice(machineConfigs, func(i, j int) bool {
		iTimestamp := machineConfigs[i].GetCreationTimestamp()
		jTimestamp := machineConfigs[j].GetCreationTimestamp()
		return jTimestamp.Before(&iTimestamp)
	})
	for _, mc := range machineConfigs {
		if strings.HasPrefix(mc.Name, RenderedWorkerPrefix) {
			if len(mc.Spec.Config.Raw) == 0 {
				continue
			}
			return &mc, nil
		}
	}
	return nil, fmt.Errorf("rendered worker MachineConfig not found")
}

// parseKubeletArgs parses a systemd unit file, returning the kubelet args WMCO is interested in
func parseKubeletArgs(unitContents string) (map[string]string, error) {
	// Remove everything before the ExecStart section of the unit file, which contains the command and args of the unit.
	// See unit test file for example systemd unit file
	execSplit := strings.SplitN(unitContents, "ExecStart=", 2)
	if len(execSplit) != 2 {
		return nil, fmt.Errorf("unit missing ExecStart")
	}
	// The ExecStart section is completed with a double newline, so using this as a split string, we can reduce the
	// scope of what we are looking at to everything inside the ExecStart section.
	cmdEndSplit := strings.SplitN(execSplit[1], "\n\n", 2)
	// Each part of the command is separated by an escaped newline
	argumentSplit := strings.Split(cmdEndSplit[0], "\\\n")
	kubeletArgs := make(map[string]string)
	// Skipping the first line, which indicates the binary, look at all the arguments which are key value pairs.
	// As WMCO currently is, we don't need to find any flags (--windows-service, for example), so we can ignore that
	// case. If there was a need for that, this logic would need to be expanded to cover that.
	windowsArgs := []string{CloudProviderOption, CloudConfigOption}
	for _, arg := range argumentSplit[1:] {
		arg = strings.TrimSpace(arg)
		arg = strings.TrimPrefix(arg, "--")
		keyValue := strings.SplitN(arg, "=", 2)
		if len(keyValue) != 2 {
			// Not a key value pair, continue
			continue
		}
		for _, windowsArg := range windowsArgs {
			if windowsArg == keyValue[0] {
				kubeletArgs[keyValue[0]] = keyValue[1]
			}
		}
	}
	return kubeletArgs, nil
}
