#!/usr/bin/env bash
#
# SPDX-FileCopyrightText: 2024 SAP SE or an SAP affiliate company and Gardener contributors
#
# SPDX-License-Identifier: Apache-2.0
set -o errexit
set -o nounset
set -o pipefail

KUBECONFIG=$1

kubectl --kubeconfig=${KUBECONFIG} apply -f ./hack/e2e-test/infrastructure/localstack/localstack.yaml --insecure-skip-tls-verify
kubectl --kubeconfig=${KUBECONFIG} rollout status deploy/localstack --insecure-skip-tls-verify
kubectl --kubeconfig=${KUBECONFIG} wait --for=condition=ready pod -l app=localstack --timeout=240s --insecure-skip-tls-verify
