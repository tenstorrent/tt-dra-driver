/*
 * Copyright The Kubernetes Authors
 * Modifications Copyright 2026 Tenstorrent USA, Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/urfave/cli/v2"

	coreclientset "k8s.io/client-go/kubernetes"
	"k8s.io/dynamic-resource-allocation/kubeletplugin"
	"k8s.io/klog/v2"

	"github.com/tenstorrent/tt-dra-driver/internal/fabricmanager"
	"github.com/tenstorrent/tt-dra-driver/internal/profiles"
	"github.com/tenstorrent/tt-dra-driver/internal/profiles/tenstorrent"
	"github.com/tenstorrent/tt-dra-driver/pkg/flags"
)

// version is the binary version, populated at build time via -ldflags.
var version = "dev"

// DriverPluginCheckpointFile is the file used to checkpoint claim state under
// the kubelet plugin data directory.
const DriverPluginCheckpointFile = "checkpoint.json"

// Flags collects all CLI flags consumed by the kubelet plugin.
type Flags struct {
	kubeClientConfig flags.KubeClientConfig
	loggingConfig    *flags.LoggingConfig

	nodeName                      string
	cdiRoot                       string
	kubeletRegistrarDirectoryPath string
	kubeletPluginsDirectoryPath   string
	healthcheckPort               int
	profile                       string
	driverName                    string
	podUID                        string
	fabricManagerAgentAddress     string
	enableTrayDevices             bool
}

// Config is the runtime configuration assembled from Flags.
type Config struct {
	flags         *Flags
	coreclient    coreclientset.Interface
	cancelMainCtx func(error)

	profile           profiles.Profile
	fabricManagerConn *fabricmanager.AgentClient
}

// validProfiles holds the set of profile names selectable via --device-profile.
var validProfiles = map[string]func(flags Flags, agent fabricmanager.TopologyClient) profiles.Profile{
	tenstorrent.ProfileName: func(f Flags, agent fabricmanager.TopologyClient) profiles.Profile {
		return tenstorrent.NewProfile(f.nodeName, agent, tenstorrent.Options{
			TrayDevices: f.enableTrayDevices,
		})
	},
}

var validProfileNames = func() []string {
	valid := make([]string, 0, len(validProfiles))
	for profileName := range validProfiles {
		valid = append(valid, profileName)
	}
	return valid
}()

// defaultDriverNameForProfile returns the canonical DRA driver name to use
// when --driver-name is not explicitly set.
func defaultDriverNameForProfile(profile string) string {
	switch profile {
	case tenstorrent.ProfileName:
		return tenstorrent.DefaultDriverName
	default:
		return profile + ".tenstorrent.com"
	}
}

// DriverPluginPath returns the per-driver plugin data directory.
func (c Config) DriverPluginPath() string {
	return filepath.Join(c.flags.kubeletPluginsDirectoryPath, c.flags.driverName)
}

func main() {
	if err := newApp().Run(os.Args); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

func newApp() *cli.App {
	flags := &Flags{
		loggingConfig: flags.NewLoggingConfig(),
	}
	cliFlags := []cli.Flag{
		&cli.StringFlag{
			Name:        "node-name",
			Usage:       "The name of the node to be worked on.",
			Required:    true,
			Destination: &flags.nodeName,
			EnvVars:     []string{"NODE_NAME"},
		},
		&cli.StringFlag{
			Name:        "cdi-root",
			Usage:       "Absolute path to the directory where CDI files will be generated.",
			Value:       "/etc/cdi",
			Destination: &flags.cdiRoot,
			EnvVars:     []string{"CDI_ROOT"},
		},
		&cli.StringFlag{
			Name:        "fabric-manager-agent-address",
			Usage:       "Address (host:port) of the Tenstorrent Fabric Manager agent on this node. The kubelet plugin uses it to discover the local ASICs.",
			Value:       "localhost:50053",
			Destination: &flags.fabricManagerAgentAddress,
			EnvVars:     []string{"FABRIC_MANAGER_AGENT_ADDRESS"},
		},
		&cli.StringFlag{
			Name:        "kubelet-registrar-directory-path",
			Usage:       "Absolute path to the directory where kubelet stores plugin registrations.",
			Value:       kubeletplugin.KubeletRegistryDir,
			Destination: &flags.kubeletRegistrarDirectoryPath,
			EnvVars:     []string{"KUBELET_REGISTRAR_DIRECTORY_PATH"},
		},
		&cli.StringFlag{
			Name:        "kubelet-plugins-directory-path",
			Usage:       "Absolute path to the directory where kubelet stores plugin data.",
			Value:       kubeletplugin.KubeletPluginsDir,
			Destination: &flags.kubeletPluginsDirectoryPath,
			EnvVars:     []string{"KUBELET_PLUGINS_DIRECTORY_PATH"},
		},
		&cli.IntFlag{
			Name:        "healthcheck-port",
			Usage:       "Port to start a gRPC healthcheck service. When positive, a literal port number. When zero, a random port is allocated. When negative, the healthcheck service is disabled.",
			Value:       -1,
			Destination: &flags.healthcheckPort,
			EnvVars:     []string{"HEALTHCHECK_PORT"},
		},
		&cli.StringFlag{
			Name:        "device-profile",
			Usage:       fmt.Sprintf("Name of the device profile. Valid values are %q.", validProfileNames),
			Value:       tenstorrent.ProfileName,
			Destination: &flags.profile,
			EnvVars:     []string{"DEVICE_PROFILE"},
		},
		&cli.StringFlag{
			Name:        "driver-name",
			Usage:       "Name of the DRA driver. Its default is derived from the device profile.",
			Destination: &flags.driverName,
			EnvVars:     []string{"DRIVER_NAME"},
		},
		&cli.BoolFlag{
			Name: "enable-tray-devices",
			Usage: "Publish one allocatable device per physical tray in addition to the per-chip devices. " +
				"Tray devices are kept mutually exclusive with the chips they contain through ResourceSlice shared counters, " +
				"which requires the DRAPartitionableDevices feature gate on the apiserver and the scheduler; " +
				"when the apiserver does not keep them, the driver logs an error and falls back to per-chip devices only.",
			Value:       true,
			Destination: &flags.enableTrayDevices,
			EnvVars:     []string{"ENABLE_TRAY_DEVICES"},
		},
		&cli.StringFlag{
			Name:        "pod-uid",
			Usage:       "UID of the pod (used for seamless upgrades to create unique socket names).",
			Destination: &flags.podUID,
			EnvVars:     []string{"POD_UID"},
		},
	}
	cliFlags = append(cliFlags, flags.kubeClientConfig.Flags()...)
	cliFlags = append(cliFlags, flags.loggingConfig.Flags()...)

	app := &cli.App{
		Name:            "tt-dra-driver-kubeletplugin",
		Usage:           "DRA kubelet plugin for Tenstorrent devices.",
		ArgsUsage:       " ",
		HideHelpCommand: true,
		Flags:           cliFlags,
		Before: func(c *cli.Context) error {
			if c.Args().Len() > 0 {
				return fmt.Errorf("arguments not supported: %v", c.Args().Slice())
			}
			return flags.loggingConfig.Apply()
		},
		Action: func(c *cli.Context) error {
			ctx := c.Context
			klog.FromContext(ctx).Info("Starting tt-dra-driver-kubeletplugin", "version", version)
			clientSets, err := flags.kubeClientConfig.NewClientSets()
			if err != nil {
				return fmt.Errorf("create client: %w", err)
			}

			if flags.driverName == "" {
				flags.driverName = defaultDriverNameForProfile(flags.profile)
			}

			newProfile, ok := validProfiles[flags.profile]
			if !ok {
				return fmt.Errorf("invalid device profile %q, valid profiles are %q", flags.profile, validProfileNames)
			}

			if flags.fabricManagerAgentAddress == "" {
				return fmt.Errorf("--fabric-manager-agent-address must be set")
			}
			// Dial does not block on connectivity, so this only records the
			// address the driver will talk to; whether the agent is actually
			// reachable first shows up on the initial GetTopology call.
			klog.FromContext(ctx).Info("Using fabric manager agent", "address", flags.fabricManagerAgentAddress)
			agentClient, err := fabricmanager.Dial(flags.fabricManagerAgentAddress)
			if err != nil {
				return fmt.Errorf("connect to fabric manager agent: %w", err)
			}

			// Confirm against the apiserver that whole-tray devices can
			// be published safely before the profile is built around
			// them.
			flags.enableTrayDevices = resolveTrayDevices(ctx, clientSets.Core, flags)

			config := &Config{
				flags:             flags,
				coreclient:        clientSets.Core,
				profile:           newProfile(*flags, agentClient),
				fabricManagerConn: agentClient,
			}

			return RunPlugin(ctx, config)
		},
	}

	return app
}

// RunPlugin starts the kubelet plugin and blocks until the context is
// cancelled, e.g. via SIGINT/SIGTERM.
func RunPlugin(ctx context.Context, config *Config) error {
	logger := klog.FromContext(ctx)

	if config.fabricManagerConn != nil {
		defer func() {
			if err := config.fabricManagerConn.Close(); err != nil {
				logger.Error(err, "Unable to close fabric manager agent connection")
			}
		}()
	}

	if err := os.MkdirAll(config.DriverPluginPath(), 0750); err != nil {
		return err
	}

	info, err := os.Stat(config.flags.cdiRoot)
	switch {
	case err != nil && os.IsNotExist(err):
		if err := os.MkdirAll(config.flags.cdiRoot, 0750); err != nil {
			return err
		}
	case err != nil:
		return err
	case !info.IsDir():
		return fmt.Errorf("path for cdi file generation is not a directory: '%v'", err)
	}

	ctx, stop := signal.NotifyContext(ctx, syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT)
	defer stop()
	ctx, cancel := context.WithCancelCause(ctx)
	config.cancelMainCtx = cancel

	driver, err := NewDriver(ctx, config)
	if err != nil {
		return err
	}

	<-ctx.Done()
	// Restore default signal behavior as soon as possible in case graceful
	// shutdown gets stuck.
	stop()
	if err := context.Cause(ctx); err != nil && !errors.Is(err, context.Canceled) {
		// A canceled context is the normal case here when the process
		// receives a signal. Only log the error for more interesting cases.
		logger.Error(err, "error from context")
	}

	if err := driver.Shutdown(logger); err != nil {
		logger.Error(err, "Unable to cleanly shutdown driver")
	}

	return nil
}
