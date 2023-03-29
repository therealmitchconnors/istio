#!/usr/bin/env bash
function kindup2() {
local ip_family
local name
local cluster_domain
local image
local compat
local multi_node


print_help() {
    printf "kindup2

Options:\n"

printf '\t%s\n' '--ip-family: networking.ipFamily to use [ipv4]'
printf '\t%s\n' '-n --name: name of the cluster [kind]'
printf '\t%s\n' '--cluster-domain: network.dnsDomain to use [cluster.local]'
printf '\t%s\n' '-i --image: node image to use [gcr.io/howardjohn-istio/kind-node:v1.26.1]'
printf '\t%s\n' '--compat: disable feature flags [off]'
printf '\t%s\n' '--multi-node: enable multiple nodes [off]'
}
die()
{
	test "${_PRINT_HELP:-no}" = yes && print_help >&2
	echo "$1" >&2
	return 1
}
parse_commandline() {
	_positionals_count=0
    _positionals=()
	while test $# -gt 0
	do
		_key="$1"
		case "$_key" in
            --ip-family)
    [[ $ip_family == "" ]] || { die "Flag already set 'ip_family'." || return 1; }
    test $# -lt 2 && { die "Missing value for argument ip-family." || return 1; }
    ip_family="$2"; shift
;;
--ip-family=*)
    [[ $ip_family == "" ]] || { die "Flag already set 'ip_family'." || return 1; }
    ip_family="${_key##--ip-family=}"
;;

-n)
    [[ $name == "" ]] || { die "Flag already set 'name'." || return 1; }
    test $# -lt 2 && { die "Missing value for argument n." || return 1; }
    name="$2"; shift
;;
-n=*)
    [[ $name == "" ]] || { die "Flag already set 'name'." || return 1; }
    name="${_key##-n=}"
;;
--name)
    [[ $name == "" ]] || { die "Flag already set 'name'." || return 1; }
    test $# -lt 2 && { die "Missing value for argument name." || return 1; }
    name="$2"; shift
;;
--name=*)
    [[ $name == "" ]] || { die "Flag already set 'name'." || return 1; }
    name="${_key##--name=}"
;;

--cluster-domain)
    [[ $cluster_domain == "" ]] || { die "Flag already set 'cluster_domain'." || return 1; }
    test $# -lt 2 && { die "Missing value for argument cluster-domain." || return 1; }
    cluster_domain="$2"; shift
;;
--cluster-domain=*)
    [[ $cluster_domain == "" ]] || { die "Flag already set 'cluster_domain'." || return 1; }
    cluster_domain="${_key##--cluster-domain=}"
;;

-i)
    [[ $image == "" ]] || { die "Flag already set 'image'." || return 1; }
    test $# -lt 2 && { die "Missing value for argument i." || return 1; }
    image="$2"; shift
;;
-i=*)
    [[ $image == "" ]] || { die "Flag already set 'image'." || return 1; }
    image="${_key##-i=}"
;;
--image)
    [[ $image == "" ]] || { die "Flag already set 'image'." || return 1; }
    test $# -lt 2 && { die "Missing value for argument image." || return 1; }
    image="$2"; shift
;;
--image=*)
    [[ $image == "" ]] || { die "Flag already set 'image'." || return 1; }
    image="${_key##--image=}"
;;

--compat)
    [[ $compat == "" ]] || { die "Flag already set 'compat'." || return 1; }
    
    compat="on"
;;
--compat=*)
    [[ $compat == "" ]] || { die "Flag already set 'compat'." || return 1; }
    compat="${_key##--compat=}"
;;

--multi-node)
    [[ $multi_node == "" ]] || { die "Flag already set 'multi_node'." || return 1; }
    
    multi_node="on"
;;
--multi-node=*)
    [[ $multi_node == "" ]] || { die "Flag already set 'multi_node'." || return 1; }
    multi_node="${_key##--multi-node=}"
;;

			-h|--help|-h*)
				print_help
                return 2
				;;
            -*)
                die "Unknown flag ${_key}"
                ;;
			*)
				_last_positional="$1"
				_positionals+=("$_last_positional")
				_positionals_count=$((_positionals_count + 1))
				;;
		esac
		shift
	done
	[[ $ip_family == "" ]] && { ip_family=ipv4; }
[[ $name == "" ]] && { name=kind; }
[[ $cluster_domain == "" ]] && { cluster_domain=cluster.local; }
[[ $image == "" ]] && { image=gcr.io/istio-testing/kind-node:v1.26.1; }
[[ $compat == "" ]] && { compat=off; }
[[ $multi_node == "" ]] && { multi_node=off; }

	return 0
}

parse_commandline "$@"
ret=$?
if [[ $ret == 2 ]]; then
    return 0
fi
if [[ $ret != 0 ]]; then
    return $ret
fi
set -- $_positionals

  # create registry container unless it already exists
  
  cidr_to_ips () {
          CIDR="$1"
          python3 - <<EOF
from ipaddress import ip_network;
from itertools import islice;
[print(str(ip) + "/" + str(ip.max_prefixlen)) for ip in islice(ip_network('$CIDR').hosts(), 100, 110)]
EOF
  }
  metallb () {
    kubectl apply -f https://raw.githubusercontent.com/metallb/metallb/v0.9.3/manifests/namespace.yaml
    # kubectl apply -f https://raw.githubusercontent.com/metallb/metallb/v0.9.3/manifests/metallb.yaml
    curl -s https://raw.githubusercontent.com/metallb/metallb/v0.9.3/manifests/metallb.yaml | sed -e 's/image: metallb/image: gcr.io\/istio-testing\/metallb/g' | kubectl apply -f -
    kubectl create secret generic -n metallb-system memberlist --from-literal=secretkey="$(openssl rand -base64 128)" || true
    METALLB_IPS="["
    for subnet in $(docker inspect kind | jq '.[0].IPAM.Config[].Subnet' -r)
    do
      while read -r ip
      do
        METALLB_IPS+="$ip,"
      done < <(cidr_to_ips "$subnet")
    done
    METALLB_IPS+="]"
    echo 'apiVersion: v1
kind: ConfigMap
metadata:
  namespace: metallb-system
  name: config
data:
  config: |
    address-pools:
    - name: default
      protocol: layer2
      addresses: '"$METALLB_IPS" | kubectl apply --kubeconfig="$KUBECONFIG" -f -
  }

  running="$(docker inspect -f '{{.State.Running}}' kind-registry)"
  if [ "${running}" != 'true' ]; then
    docker run \
      -d --restart=always -p 127.0.0.1:5000:5000 --name kind-registry \
      registry:2
    # connect the registry to the cluster network
    docker network connect kind kind-registry
  fi
  FEATURE_GATES=yes
  if [[ "${compat}" == "on" ]]; then FEATURE_GATES=""; fi
  MULTI_NODE=yes
  if [[ "${multi_node}" == "off" ]]; then MULTI_NODE=""; fi

  cat <<EOF | kind create cluster --retain --name ${name} --image "${image}" --config=- || return 1
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
${FEATURE_GATES:+
featureGates:
  MixedProtocolLBService: true
  EphemeralContainers: true
}
kubeadmConfigPatches:
  - |
    apiVersion: kubeadm.k8s.io/v1beta2
    kind: ClusterConfiguration
    metadata:
      name: config
    etcd:
      local:
        # Run etcd in a tmpfs (in RAM) for performance improvements
        dataDir: /tmp/kind-cluster-etcd
    controllerManager:
      extraArgs:
        "kube-api-burst": "500"
        "kube-api-qps": "250"
    apiServer:
      extraArgs:
        "service-account-issuer": "kubernetes.default.svc"
        "service-account-signing-key-file": "/etc/kubernetes/pki/sa.key"
    networking:
      dnsDomain: ${cluster_domain}
containerdConfigPatches: 
- |-
  [plugins."io.containerd.grpc.v1.cri".registry.mirrors."localhost:5000"]
    endpoint = ["http://kind-registry:5000"]
networking:
  ipFamily: ${ip_family}
  disableDefaultCNI: ${DISABLE_CNI:-false}
  kubeProxyMode: ${KUBE_PROXY:-iptables}
${MULTI_NODE:+
nodes:
- role: control-plane
- role: worker
  labels:
    topology.kubernetes.io/region: us
- role: worker
  labels:
    topology.kubernetes.io/region: eu
}
EOF
  CONTAINER_ID=$(cat /etc/hostname)
  if docker inspect $CONTAINER_ID; then
    # our dev env is a container, make sure it is connected to kind network
    kind export kubeconfig --internal
    KIND_ADDRESS=$(docker inspect $CONTAINER_ID  --format "{{ .NetworkSettings.Networks.kind.IPAddress }}")
    if [ "$KIND_ADDRESS" == "<no value>" ]; then
      docker network connect kind $CONTAINER_ID
    fi
  fi
  # REGISTRY_IP=$(docker inspect kind-registry  --format "{{ .NetworkSettings.Networks.kind.IPAddress }}")
  docker images $HUB/* --format "{{.Repository}}:{{.Tag}}" | sed s/$HUB//g | xargs -I{} docker tag $HUB{} localhost:5000{}
  docker images localhost:5000/* --format "{{.Repository}}:{{.Tag}}" | xargs -I{} docker push {}
  # docker exec ${name}-control-plane bash -c "sysctl -w kernel.core_pattern=/var/lib/istio/data/core.proxy && ulimit -c unlimited"
  kubectl label namespace default istio-injection=enabled || true
  metallb
  

}
kindup2 "$@"