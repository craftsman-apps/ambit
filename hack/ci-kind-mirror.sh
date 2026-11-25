#!/usr/bin/env bash
# CI only (#323, after #293). Docker Hub images on kind nodes come through Google's mirror (mirror.gcr.io), and
# through Docker Hub itself if the mirror fails. The CI jobs that create kind clusters (ambit local up, kind create
# cluster in a test script) cannot take a kind config from us, and kind/cluster.yaml and `ambit local up` must stay
# as users get them. So this script runs in the background for the whole job and writes the mirror file into every
# kind node container soon after it starts. The node's containerd reads that file at each pull. The images are
# pinned by digest, so the mirror cannot change them. A pull that starts before the file exists goes to Docker Hub.
# Usage: hack/ci-kind-mirror.sh [cluster-name]   (no name: every kind cluster; a name is for trying it on a machine
# that has other clusters)
set -u
label=io.x-k8s.kind.cluster${1:+=$1}
seen=" "
while :; do
  for id in $(docker ps --filter "label=$label" --format '{{.ID}}' 2>/dev/null); do
    case "$seen" in *" $id "*) continue ;; esac
    if docker exec "$id" sh -c 'mkdir -p /etc/containerd/certs.d/docker.io && printf "%s\n" \
        "server = \"https://registry-1.docker.io\"" "[host.\"https://mirror.gcr.io\"]" "  capabilities = [\"pull\", \"resolve\"]" \
        > /etc/containerd/certs.d/docker.io/hosts.toml' 2>/dev/null; then
      echo "mirror set on kind node $id"
      seen="$seen$id "
    fi
  done
  sleep 1
done
