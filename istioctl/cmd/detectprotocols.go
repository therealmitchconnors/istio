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
	"github.com/hashicorp/go-multierror"
	"github.com/prometheus/common/model"
	"istio.io/istio/istioctl/pkg/clioptions"
	"istio.io/istio/pkg/kube"
	"istio.io/pkg/log"
	v12 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"strings"
	"time"

	prometheus "github.com/prometheus/client_golang/api"
	v1 "github.com/prometheus/client_golang/api/prometheus/v1"
	"github.com/spf13/cobra"
)

func protocolLabelsCmd() *cobra.Command {
	var opts clioptions.ControlPlaneOptions
	var promURI string
	// protocolLabelsCmd represents the protocolLabels command
	var cmd = &cobra.Command{
		Use:   "detect-protocols",
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

			svcproto, err := getSvcProtocols(client, promURI)
			if err != nil {
				return fmt.Errorf("failed to query protocols: %v", err)
			}

			namespaces := make(map[string]struct{})
			for svc := range svcproto {
				ns := strings.Split(svc, ".")[1]
				namespaces[ns] = struct{}{}
			}
			var errs error
			for ns := range namespaces {
				services, err := client.Kube().CoreV1().Services(ns).List(context.TODO(), v12.ListOptions{})
				if err != nil {
					errs = multierror.Append(errs, err)
				}
				for _, svc := range services.Items {
					servicename := fmt.Sprintf("%s.%s.svc.cluster.local", svc.Name, ns)
					if len(svc.Spec.Ports) == 1 {
						protocols := svcproto[servicename]
						if len(protocols) == 1 && !strings.HasPrefix(svc.Spec.Ports[0].Name, protocols[0]) {
							fmt.Printf("Consider naming the only port on service %s.%s to %s\n", svc.Name, ns, protocols[0])
						}
					}
				}
			}
			if errs != nil {
				fmt.Println("WARNING: not all namespaces could be checked:")
				fmt.Printf("%v", errs)
			}
			return nil
		},
	}
	cmd.PersistentFlags().StringVar(&promURI, "prometheus-uri", "", "The URI of the prometheus instance " +
		"which is recording telemetry for the mesh.  Overrides port forwarding behavior")
	return cmd
}

func getSvcProtocols(client kube.ExtendedClient, promURI string) (map[string][]string, error) {
	pl, err := client.PodsForSelector(context.TODO(), addonNamespace, "app=prometheus")
	if err != nil {
		return nil, fmt.Errorf("not able to locate Prometheus pod: %v", err)
	}

	if len(pl.Items) < 1 {
		return nil, errors.New("no Prometheus pods found")
	}

	var addr string
	if len(promURI) < 1 {
		// only use the first pod in the list
		addr, err = portForwardAsync(pl.Items[0].Name, addonNamespace, "Prometheus", bindAddress, 9090, client)
		if err != nil {
			return nil, fmt.Errorf("not able to forward to Prometheus: %v", err)
		}
	} else {
		addr = promURI
	}

	pclient, err := prometheus.NewClient(prometheus.Config{
		Address:      fmt.Sprintf("http://%s", addr),
	})

	if err != nil {
		return nil, fmt.Errorf("could not connect to Prometheus via port forwarding: %v", err)
	}

	prom := v1.NewAPI(pclient)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	result, warnings, err := prom.Query(ctx, "sum by (destination_service, request_protocol) (istio_requests_total)", time.Now())
	if err != nil {
		return nil, fmt.Errorf("Error querying Prometheus: %v\n", err)
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
	return svcproto, nil
}

// portForwardAsync first tries to forward localhost:remotePort to podName:remotePort, falls back to dynamic local port
func portForwardAsync(podName, namespace, flavor, localAddress string, remotePort int, client kube.ExtendedClient) (string, error) {
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
