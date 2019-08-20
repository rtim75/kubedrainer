package main

import (
	"time"

	"github.com/VirtusLab/kubedrainer/internal/settings"
	"github.com/VirtusLab/kubedrainer/pkg/drainer"
	"github.com/VirtusLab/kubedrainer/pkg/kubernetes"
	"github.com/VirtusLab/kubedrainer/pkg/kubernetes/node"
	"github.com/VirtusLab/kubedrainer/pkg/trigger/aws"
	"github.com/VirtusLab/kubedrainer/pkg/trigger/aws/autoscaling"
	"github.com/VirtusLab/kubedrainer/pkg/trigger/aws/metadata"

	"github.com/VirtusLab/go-extended/pkg/errors"
	"github.com/VirtusLab/go-extended/pkg/matcher"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/golang/glog"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"github.com/spf13/viper"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	k8s "k8s.io/client-go/kubernetes"
)

type ServeOptions struct {
	Kubernetes *kubernetes.Options
	Drainer    *drainer.Options
	AWS        *autoscaling.Options
}

type ServeFlags struct {
	Kubernetes *pflag.FlagSet
	Drainer    *pflag.FlagSet
	AWS        *pflag.FlagSet
}

// serveCmd represents the serve command
func serveCmd() *cobra.Command {
	options := &ServeOptions{
		Kubernetes: genericclioptions.NewConfigFlags(true),
		Drainer: &drainer.Options{
			GracePeriodSeconds:  -1,
			Timeout:             60 * time.Second,
			DeleteLocalData:     true,
			IgnoreAllDaemonSets: true,
		},
		AWS: &autoscaling.Options{
			LoopSleepTime: 10 * time.Second,
			ShutdownSleep: 6 * time.Minute,
		},
	}

	flags := &ServeFlags{
		Kubernetes: kubernetesFlags(options.Kubernetes),
		Drainer:    drainerFlags(options.Drainer),
		AWS:        autoscalingFlags(options.AWS),
	}

	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run node drainer as server",
		Long:  `Run node drainer as server with the provided configuration`,
		RunE: func(cmd *cobra.Command, args []string) error {
			glog.Info("Running as server")

			if err := options.Parse(cmd); err != nil {
				return err
			}

			kubernetesClient, err := kubernetes.New(options.Kubernetes)
			if err != nil {
				return err
			}

			awsSession, _, err := aws.SessionConfig(options.AWS.Region, options.AWS.Profile)
			if err != nil {
				return err
			}

			// get information from Kubernetes API if necessary
			if len(options.AWS.Region) == 0 && len(options.AWS.InstanceID) == 0 {
				glog.V(1).Info("Getting node information")
				region, instanceID, err := GetNodeInformation(options.Drainer.Node, kubernetesClient)
				if err != nil {
					return err
				}
				if len(options.AWS.InstanceID) == 0 {
					options.AWS.InstanceID = instanceID
				} else {
					glog.V(1).Infof("Ignoring instance ID from node info '%s', using current: '%s'",
						instanceID, options.AWS.InstanceID)
				}
				if len(options.AWS.Region) == 0 {
					options.AWS.Region = region
				} else {
					glog.V(1).Infof("Ignoring region from node info '%s', using current: '%s'",
						instanceID, options.AWS.Region)
				}
			}

			// get information from AWS API if necessary
			if len(options.AWS.Region) == 0 && len(options.AWS.InstanceID) == 0 {
				glog.V(1).Info("Getting EC2 metadata")
				region, instanceID, err := GetMetadata(awsSession)
				if err != nil {
					return err
				}
				if len(options.AWS.InstanceID) == 0 {
					options.AWS.InstanceID = instanceID
				} else {
					glog.V(1).Infof("Ignoring instance ID from metadata '%s', using current: '%s'",
						instanceID, options.AWS.InstanceID)
				}
				if len(options.AWS.Region) == 0 {
					options.AWS.Region = region
				} else {
					glog.V(1).Infof("Ignoring region from metadata '%s', using current: '%s'",
						instanceID, options.AWS.Region)
				}
			}

			if len(options.AWS.Profile) == 0 {
				glog.V(1).Infof("Using default AWS API credentials profile")
				options.AWS.Profile = aws.DefaultProfile
			}

			// check preconditions
			if len(options.AWS.InstanceID) == 0 {
				return errors.New("No instance ID from flags, configuration, or metadata")
			}
			if len(options.AWS.Region) == 0 {
				return errors.New("No region from flags, configuration, or metadata")
			}
			if len(options.AWS.Profile) == 0 {
				return errors.New("No profile provided")
			}
			if len(options.Drainer.Node) == 0 {
				return errors.New("No node name provided")
			}

			t := aws.HookHandler{
				Drainer:     drainer.New(kubernetesClient, options.Drainer),
				AutoScaling: autoscaling.New(awsSession, options.AWS),
			}

			t.Loop(options.Drainer.Node)

			return errors.Wrap(err)
		},
	}

	flags.AddTo(cmd.PersistentFlags())
	return cmd
}

func (f *ServeFlags) AddTo(flags *pflag.FlagSet) {
	flags.AddFlagSet(f.Kubernetes)
	flags.AddFlagSet(f.Drainer)
	flags.AddFlagSet(f.AWS)
}

func (o *ServeOptions) Parse(cmd *cobra.Command) error {
	settings.Bind(cmd.Flags()) // needs to be run inside the command and before any viper usage for flags to be visible

	glog.V(4).Infof("All keys: %+v", viper.AllKeys())
	glog.V(2).Infof("All settings: %+v", viper.AllSettings())
	if glog.V(4) {
		cmd.Flags().VisitAll(func(flag *pflag.Flag) {
			glog.Infof("'%s' -> flag: '%+v' | setting: '%+v'", flag.Name, flag.Value, viper.Get(flag.Name))
		})
	}
	glog.V(1).Infof("Settings: %+v", *o)

	if err := settings.Parse(o.Kubernetes); err != nil {
		return err
	}
	if err := settings.Parse(o.Drainer); err != nil {
		return err
	}
	if err := settings.Parse(o.AWS); err != nil {
		return err
	}
	return nil
}

// GetNodeInformation gets region and instance ID for a given node from Kubernetes API
func GetNodeInformation(nodeName string, kubernetesClient k8s.Interface) (string, string, error) {
	var region string
	var instanceID string

	n := &node.Node{
		Client: kubernetesClient,
	}
	providerName, providerSpecificNodeID, err := n.GetProviderID(nodeName)
	if err != nil {
		return "", "", err
	}
	switch providerName {
	case "aws":
		awsNodeIDExpression := `^/(?P<Region>[a-zA-Z0-9-]+)[a-z]/(?P<InstanceID>[a-zA-Z0-9-]+)$`
		results, ok := matcher.Must(awsNodeIDExpression).MatchGroups(providerSpecificNodeID)
		if !ok {
			return "", "", errors.Errorf("Can't match expression '%s' to '%s'",
				awsNodeIDExpression, providerSpecificNodeID)
		}
		region, ok = results["Region"]
		if !ok {
			return "", "", errors.Errorf("Missing 'Region' when expression '%s' was applied to '%s'",
				awsNodeIDExpression, providerSpecificNodeID)
		}
		instanceID, ok = results["InstanceID"]
		if !ok {
			return "", "", errors.Errorf("Missing 'InstanceID' when expression '%s' was applied to '%s'",
				awsNodeIDExpression, providerSpecificNodeID)
		}
	default:
		return "", "", errors.Errorf("Cloud provider not supported: '%s'", providerName)
	}

	return region, instanceID, nil
}

// GetMetadata gets region and instance ID from EC2 instance metadata
func GetMetadata(awsSession *session.Session) (string, string, error) {
	var region string
	var instanceID string
	m := metadata.New(awsSession)
	instanceID, region, err := m.GetCurrentInstanceIDAndRegion()
	switch err := err.(type) {
	case nil: // nothing
	case awserr.Error:
		if err.Code() == "EC2MetadataRequestError" {
			glog.Warning("No EC2 metadata available")
		} else {
			return "", "", errors.Wrap(err)
		}
	default:
		return "", "", errors.Wrap(err)
	}
	return region, instanceID, nil
}
