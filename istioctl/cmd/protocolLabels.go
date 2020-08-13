// Copyright © 2020 NAME HERE <EMAIL ADDRESS>
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package cmd

import (
	"context"
	"errors"
	"fmt"
	"github.com/prometheus/common/model"
	"io"
	"istio.io/istio/istioctl/pkg/clioptions"
	"istio.io/istio/pkg/kube"
	"istio.io/pkg/log"
	"time"

	"github.com/spf13/cobra"
	prometheus "github.com/prometheus/client_golang/api"
	v1 "github.com/prometheus/client_golang/api/prometheus/v1"
)

func protocolLabelsCmd() *cobra.Command {
	var opts clioptions.ControlPlaneOptions
	// protocolLabelsCmd represents the protocolLabels command
	var cmd = &cobra.Command{
		Use:   "protocolLabels",
		Short: "A brief description of your command",
		Long: `A longer description that spans multiple lines and likely contains examples
and usage of using your command. For example:

Cobra is a CLI library for Go that empowers applications.
This application is a tool to generate the needed files
to quickly create a Cobra application.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := kubeClientWithRevision(kubeconfig, configContext, opts.Revision)
			if err != nil {
				return fmt.Errorf("failed to create k8s client: %v", err)
			}

			pl, err := client.PodsForSelector(context.TODO(), addonNamespace, "app=prometheus")
			if err != nil {
				return fmt.Errorf("not able to locate Prometheus pod: %v", err)
			}

			if len(pl.Items) < 1 {
				return errors.New("no Prometheus pods found")
			}

			// only use the first pod in the list
			addr, err := portForwardAsync(pl.Items[0].Name, addonNamespace, "Prometheus",
				"http://%s", bindAddress, 9090, client, cmd.OutOrStdout())

			if err != nil {
				return fmt.Errorf("not able to forward to Prometheus: %v", err)
			}

			pclient, err := prometheus.NewClient(prometheus.Config{
				Address:      fmt.Sprintf("http://%s", addr),
			})

			if err != nil {
				return fmt.Errorf("could not connect to Prometheus via port forwarding: %v", err)
			}

			prom := v1.NewAPI(pclient)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			result, warnings, err := prom.Query(ctx, "sum by (destination_service, request_protocol) (istio_requests_total)", time.Now())
			if err != nil {
				return fmt.Errorf("Error querying Prometheus: %v\n", err)
			}
			if len(warnings) > 0 {
				fmt.Printf("Warnings: %v\n", warnings)
			}
			//fmt.Printf("Result:\n%v\n", result)
			rv := result.(model.Vector)
			svcproto := make(map[string][]string)
			for _, i := range rv {
				svc, ok := i.Metric["destination_service"]
				if !ok {
					fmt.Printf("WARNING: no destination_service in %v/n", i)
				}
				protocol, ok := i.Metric["request_protocol"]
				if !ok {
					fmt.Printf("WARNING: no request_protocol in %v/n", i)
				}
				svcproto[string(svc)] = append(svcproto[string(svc)], string(protocol))
			}
			for svc, protocols := range svcproto {
				if len(protocols) == 1 {
					fmt.Printf("Service %s uses only protocol %s\n", svc, protocols[0])
				}
			}
			return nil
		},
	}
	return cmd
}

// portForwardAsync first tries to forward localhost:remotePort to podName:remotePort, falls back to dynamic local port
func portForwardAsync(podName, namespace, flavor, urlFormat, localAddress string, remotePort int,
	client kube.ExtendedClient, writer io.Writer) (string, error) {

	// port preference:
	// - If --listenPort is specified, use it
	// - without --listenPort, prefer the remotePort but fall back to a random port
	var portPrefs []int
	if listenPort != 0 {
		portPrefs = []int{listenPort}
	} else {
		portPrefs = []int{remotePort, 0}
	}

	var err error
	for _, localPort := range portPrefs {
		fw, err := client.NewPortForwarder(podName, namespace, localAddress, localPort, remotePort)
		if err != nil {
			return "", fmt.Errorf("could not build port forwarder for %s: %v", flavor, err)
		}

		if err = fw.Start(); err != nil {
			// Try the next port
			continue
		}

		// Close the port forwarder when the command is terminated.
		closePortForwarderOnInterrupt(fw)

		log.Debugf(fmt.Sprintf("port-forward to %s pod ready", flavor))

		return fw.Address(), nil
	}

	return "", fmt.Errorf("failure running port forward: %v", err)
}

func init() {
	// Here you will define your flags and configuration settings.

	// Cobra supports Persistent Flags which will work for this command
	// and all subcommands, e.g.:
	// protocolLabelsCmd.PersistentFlags().String("foo", "", "A help for foo")

	// Cobra supports local flags which will only run when this command
	// is called directly, e.g.:
	// protocolLabelsCmd.Flags().BoolP("toggle", "t", false, "Help message for toggle")
}
