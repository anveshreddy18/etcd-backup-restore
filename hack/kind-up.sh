#!/usr/bin/env bash
#
# SPDX-FileCopyrightText: 2024 SAP SE or an SAP affiliate company and Gardener contributors
#
# SPDX-License-Identifier: Apache-2.0


set -o errexit
set -o nounset
set -o pipefail

kind create cluster --name etcdbr-e2e --config hack/e2e-test/infrastructure/kind/cluster.yaml

# sleep for 5 seconds to allow the creation of kubeconfig file at hack/e2e-test/infrastructure/kind/kubeconfig
sleep 5

# Modify the kubeconfig file to update the server field
modify_kubeconfig() {
  kubeconfig_path="hack/e2e-test/infrastructure/kind/kubeconfig"
  if [[ -f "$kubeconfig_path" ]]; then
    echo "Modifying kubeconfig file at $kubeconfig_path"
    python3 - <<EOF
import yaml

kubeconfig_path = "$kubeconfig_path"

with open(kubeconfig_path, "r") as f:
    data = yaml.safe_load(f)

# Update the server field
server_url = data["clusters"][0]["cluster"]["server"]
if "127.0.0.1" in server_url:
    port = server_url.split(":")[-1]
    data["clusters"][0]["cluster"]["server"] = f"https://host.docker.internal:{port}"

with open(kubeconfig_path, "w") as f:
    yaml.safe_dump(data, f)

print(f"Updated server field in {kubeconfig_path}")
EOF
  else
    echo "Kubeconfig file not found at $kubeconfig_path"
    exit 1
  fi
}

# Call the function to modify the kubeconfig
modify_kubeconfig
