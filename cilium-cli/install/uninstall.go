// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of Cilium

package install

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"helm.sh/helm/v3/pkg/action"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/cilium/cilium/cilium-cli/defaults"
	pkgk8s "github.com/cilium/cilium/pkg/k8s"
)

type UninstallParameters struct {
	Namespace            string
	TestNamespace        string
	Writer               io.Writer
	Wait                 bool
	HelmValuesSecretName string
	RedactHelmCertKeys   bool
	HelmChartDirectory   string
	WorkerCount          int
	Timeout              time.Duration
	HelmReleaseName      string
}

type K8sUninstaller struct {
	client k8sInstallerImplementation
	params UninstallParameters
}

func NewK8sUninstaller(client k8sInstallerImplementation, p UninstallParameters) *K8sUninstaller {
	return &K8sUninstaller{
		client: client,
		params: p,
	}
}

func (k *K8sUninstaller) Log(format string, a ...any) {
	fmt.Fprintf(k.params.Writer, format+"\n", a...)
}

// DeleteTestNamespace deletes all pods in the test namespace and then deletes the namespace itself.
func (k *K8sUninstaller) DeleteTestNamespace(ctx context.Context) {
	k.Log("🔥 Deleting pods in %s namespace...", k.params.TestNamespace)
	k.client.DeletePodCollection(ctx, k.params.TestNamespace, metav1.DeleteOptions{}, metav1.ListOptions{})

	k.Log("🔥 Deleting %s namespace...", k.params.TestNamespace)
	k.client.DeleteNamespace(ctx, k.params.TestNamespace, metav1.DeleteOptions{})

	// If test Pods are not deleted prior to uninstalling Cilium then the CNI deletes
	// may be queued by cilium-cni. This can cause error to be logged when re-installing
	// Cilium later.
	// Thus we wait for all cilium-test Pods to fully terminate before proceeding.
	if k.params.Wait {
		k.Log("⌛ Waiting for %s namespace to be terminated...", k.params.TestNamespace)
		for {
			// Wait for the test namespace to be terminated. Subsequent connectivity checks would fail
			// if the test namespace is in Terminating state.
			_, err := k.client.GetNamespace(ctx, k.params.TestNamespace, metav1.GetOptions{})
			if err == nil {
				time.Sleep(defaults.WaitRetryInterval)
			} else {
				break
			}
		}
	}
}

func (k *K8sUninstaller) cleanupNodeAnnotations(ctx context.Context) error {
	k.Log("🔥 Cleaning up Cilium node annotations...")

	nodes, err := k.client.ListNodes(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("failed to list nodes: %w", err)
	}

	for _, node := range nodes.Items {
		annotations := make(map[string]string)
		for key := range node.Annotations {
			if strings.HasPrefix(key, "io.cilium.") || strings.HasPrefix(key, "cilium.io/") {
				annotations[key] = node.Annotations[key]
			}
		}

		if len(annotations) > 0 {
			k.Log("  Removing annotations from node %s", node.Name)
			patch := []pkgk8s.JSONPatch{}
			for key := range annotations {
				escapedKey := strings.ReplaceAll(key, "~", "~0")
				escapedKey = strings.ReplaceAll(escapedKey, "/", "~1")
				patch = append(patch, pkgk8s.JSONPatch{
					OP:   "remove",
					Path: "/metadata/annotations/" + escapedKey,
				})
			}
			patchBytes, err := json.Marshal(patch)
			if err != nil {
				return fmt.Errorf("failed to marshal patch for node %s: %w", node.Name, err)
			}
			_, err = k.client.PatchNode(ctx, node.Name, types.JSONPatchType, patchBytes)
			if err != nil {
				return fmt.Errorf("failed to patch node %s: %w", node.Name, err)
			}
		}
	}

	return nil
}

func (k *K8sUninstaller) UninstallWithHelm(ctx context.Context, actionConfig *action.Configuration) error {
	// First, delete test namespace and wait for it to terminate (if Wait is set)
	k.DeleteTestNamespace(ctx)
	helmClient := action.NewUninstall(actionConfig)
	helmClient.Wait = k.params.Wait
	if k.params.Wait {
		helmClient.DeletionPropagation = "foreground"
	}
	helmClient.Timeout = k.params.Timeout
	if _, err := helmClient.Run(k.params.HelmReleaseName); err != nil {
		return err
	}
	// Clean up node annotations
	if err := k.cleanupNodeAnnotations(ctx); err != nil {
		k.Log("Failed to clean up node annotations: %v", err)
	}
	// If aws-node daemonset exists, remove io.cilium/aws-node-enabled node selector.
	if _, err := k.client.GetDaemonSet(ctx, AwsNodeDaemonSetNamespace, AwsNodeDaemonSetName, metav1.GetOptions{}); err != nil {
		return nil
	}
	return k.undoAwsNodeNodeSelector(ctx)
}

func (k *K8sUninstaller) undoAwsNodeNodeSelector(ctx context.Context) error {
	bytes := fmt.Appendf(nil, `[{"op":"remove","path":"/spec/template/spec/nodeSelector/%s"}]`, strings.ReplaceAll(AwsNodeDaemonSetNodeSelectorKey, "/", "~1"))
	k.Log("⏪ Undoing the changes to the %q DaemonSet...", AwsNodeDaemonSetName)
	_, err := k.client.PatchDaemonSet(ctx, AwsNodeDaemonSetNamespace, AwsNodeDaemonSetName, types.JSONPatchType, bytes, metav1.PatchOptions{})
	if err != nil {
		k.Log("❌ Failed to patch the %q DaemonSet, please remove it's node selector manually", AwsNodeDaemonSetName)
	}
	return err
}
