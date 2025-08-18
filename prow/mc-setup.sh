#!/bin/bash

# Copyright 2019 Istio Authors
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#    http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.


# Usage: ./integ-suite-kind.sh TARGET
# Example: ./integ-suite-kind.sh test.integration.pilot.kube.presubmit

WD=$(dirname "$0")
WD=$(cd "$WD"; pwd)
ROOT=$(dirname "$WD")

# Exit immediately for non zero status
set -e
# Check unset variables
set -u
# Print commands
set -x

# shellcheck source=prow/lib.sh
source "${ROOT}/prow/lib.sh"
setup_and_export_git_sha

# shellcheck source=common/scripts/kind_provisioner.sh
source "${ROOT}/common/scripts/kind_provisioner.sh"

TOPOLOGY=SINGLE_CLUSTER
NODE_IMAGE="gcr.io/istio-testing/kind-node:v1.32.0"
KIND_CONFIG=""
CLUSTER_TOPOLOGY_CONFIG_DIR="$1"
ARTIFACTS=$CLUSTER_TOPOLOGY_CONFIG_DIR
CLUSTER_TOPOLOGY_CONFIG_FILE="${CLUSTER_TOPOLOGY_CONFIG_DIR}/topology-config.json"
CLUSTER_NAME="${CLUSTER_NAME:-istio-testing}"

trace "load cluster topology" load_cluster_topology "${CLUSTER_TOPOLOGY_CONFIG_FILE}"

export CLUSTER_KUBECONFIGS

KUBE_CLUSTERS=$(jq '.[] | select(.kind == "Kubernetes" or .kind == null)' "${CLUSTER_TOPOLOGY_CONFIG_FILE}")

while read -r value; do
  CLUSTER_KUBECONFIGS+=("$value")
done < <(echo "${KUBE_CLUSTERS}" | jq -r '.meta.kubeconfig')


echo "fhqwgads"
mkdir "${ARTIFACTS}/certs" || true
pushd "${ARTIFACTS}/certs"
make -f ${ROOT}/tools/certs/Makefile.selfsigned.mk root-ca
export CLUSTER_SECRETS

ITER_END=$((NUM_CLUSTERS-1))
for i in $(seq 0 "$ITER_END"); do
  c="${CLUSTER_NAMES[i]}"
  kc="--kubeconfig ${CLUSTER_KUBECONFIGS[i]}"
  # setup trust
  make -f ${ROOT}/tools/certs/Makefile.selfsigned.mk "${c}-cacerts"
  kubectl $kc get ns istio-system &> /dev/null || \
    { kubectl create ns istio-system $kc; }
  kubectl label ns istio-system $kc --overwrite topology.istio.io/network=${CLUSTER_NETWORK_ID[i]}

  kubectl $kc get secret -n istio-system cacerts &> /dev/null || \
    { kubectl create secret generic cacerts -n istio-system $kc \
        --from-file=${c}/ca-cert.pem \
        --from-file=${c}/ca-key.pem \
        --from-file=${c}/root-cert.pem \
        --from-file=${c}/cert-chain.pem; }

  # install istio
  istioctl install -f ${ROOT}/mc-scale-operator.yaml -y $kc \
    --set values.global.multiCluster.clusterName=${c} --set values.global.network=${CLUSTER_NETWORK_ID[i]}

  # install e/w gateways
  kubectl get crd gateways.gateway.networking.k8s.io &> /dev/null || \
    kubectl kustomize "github.com/kubernetes-sigs/gateway-api/config/crd?ref=v1.3.0" | kubectl apply $kc -f -
  ${ROOT}/samples/multicluster/gen-eastwest-gateway.sh \
    --network "${CLUSTER_NETWORK_ID[i]}" \
    --ambient | \
    kubectl apply $kc -f -

  # add to remote cluster list
  SECRET=$(istioctl create-remote-secret $kc --name $c)
  CLUSTER_SECRETS+=("$SECRET")

  # add global sample service and client
  kubectl $kc get ns sample &> /dev/null || \
    { kubectl create ns sample $kc; }
  kubectl $kc label ns sample istio.io/dataplane-mode=ambient
  # kubectl apply -f ${ROOT}/samples/helloworld/helloworld.yaml \
  #   -l service=helloworld -n sample $kc
  ${ROOT}/samples/helloworld/gen-helloworld.sh \
    --version ${c} | \
    kubectl apply -n sample $kc -f -
  kubectl label svc helloworld -n sample 'istio.io/global=true' $kc
  kubectl apply -f ${ROOT}/samples/curl/curl.yaml -n sample $kc
done

for o in $(seq 0 "$ITER_END"); do
  for i in $(seq 0 "$ITER_END"); do
    if [[ $i != $o ]]; then
      echo "${CLUSTER_SECRETS[i]}" | kubectl apply --kubeconfig ${CLUSTER_KUBECONFIGS[o]} -f -
    fi
  done
done
popd

for i in $(seq 0 "$ITER_END"); do

  kc="--kubeconfig ${CLUSTER_KUBECONFIGS[i]}"
  kubectl $kc get ns pilot-load &> /dev/null || \
    { kubectl create ns pilot-load $kc; }
  kubectl $kc get cm -n pilot-load pilot-load-config &> /dev/null || \
    { kubectl $kc create configmap --from-file config.yaml=load-examples/ambient.yaml pilot-load-config -n pilot-load; }
  kubectl $kc apply -f load-examples/deployment.yaml

done