/*
Copyright The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package e2enode

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/kubernetes/pkg/features"
	"k8s.io/kubernetes/pkg/kubelet/lifecycle"
	"k8s.io/kubernetes/test/e2e/framework"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
	e2eskipper "k8s.io/kubernetes/test/e2e/framework/skipper"
	imageutils "k8s.io/kubernetes/test/utils/image"
	admissionapi "k8s.io/pod-security-admission/api"
	"k8s.io/utils/ptr"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
)

// cgroupOptionsFeatureName is the name the kubelet declares in
// node.status.declaredFeatures once writable cgroups are usable on the node.
const cgroupOptionsFeatureName = "CgroupOptions"

// mkdirCmd creates and removes a descendant cgroup, which only succeeds when
// /sys/fs/cgroup is mounted read-write for the container.
var mkdirCmd = []string{"sh", "-c", "mkdir /sys/fs/cgroup/e2e-test && rmdir /sys/fs/cgroup/e2e-test"}

// expectCgroupReadOnly asserts that the container cannot create a cgroup, and
// that the reason is the read-only mount rather than any other exec failure.
func expectCgroupReadOnly(f *framework.Framework, podName, containerName string) {
	ginkgo.GinkgoHelper()
	stdout, stderr, err := e2epod.ExecCommandInContainerWithFullOutput(f, podName, containerName, mkdirCmd...)
	gomega.Expect(err).To(gomega.HaveOccurred(), "expected mkdir in read-only /sys/fs/cgroup to fail; stdout=%q stderr=%q", stdout, stderr)
	gomega.Expect(stderr).To(gomega.ContainSubstring("Read-only file system"), "expected mkdir to fail because /sys/fs/cgroup is read-only")
}

func nodeDeclaresCgroupOptions(ctx context.Context, f *framework.Framework) bool {
	nodeList, err := f.ClientSet.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	framework.ExpectNoError(err)
	// Assuming that there is only one node, because this is a node e2e test.
	gomega.Expect(nodeList.Items).To(gomega.HaveLen(1))
	return slices.Contains(nodeList.Items[0].Status.DeclaredFeatures, cgroupOptionsFeatureName)
}

func cgroupOptionsContainer(name string, mountMode *v1.CgroupMountMode) v1.Container {
	sc := &v1.SecurityContext{}
	if mountMode != nil {
		sc.CgroupOptions = &v1.CgroupOptions{MountMode: mountMode}
	}
	return v1.Container{
		Name:            name,
		Image:           imageutils.GetE2EImage(imageutils.BusyBox),
		Command:         []string{"/bin/sleep", "10000"},
		SecurityContext: sc,
	}
}

func cgroupOptionsPod(name string, containers ...v1.Container) *v1.Pod {
	return &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: v1.PodSpec{
			RestartPolicy: v1.RestartPolicyNever,
			Containers:    containers,
		},
	}
}

var _ = SIGDescribe("CgroupOptions", framework.WithFeatureGate(features.CgroupOptions), func() {
	f := framework.NewDefaultFramework("cgroup-options-test")
	f.NamespacePodSecurityLevel = admissionapi.LevelPrivileged
	var podClient *e2epod.PodClient

	ginkgo.BeforeEach(func(ctx context.Context) {
		podClient = e2epod.NewPodClient(f)
	})

	ginkgo.Context("when the node declares support for writable cgroups", func() {
		ginkgo.BeforeEach(func(ctx context.Context) {
			if !IsCgroup2UnifiedMode() {
				ginkgo.Skip("This test requires cgroups v2")
			}
			waitForNodeReady(ctx)
			if !nodeDeclaresCgroupOptions(ctx, f) {
				e2eskipper.Skipf("node does not declare the %s feature", cgroupOptionsFeatureName)
			}
		})

		ginkgo.It("should mount /sys/fs/cgroup writable when mountMode is Writable", func(ctx context.Context) {
			pod := cgroupOptionsPod("cgroup-writable",
				cgroupOptionsContainer("test", ptr.To(v1.CgroupMountModeWritable)))
			podClient.CreateSync(ctx, pod)

			ginkgo.By("verifying the container can create a descendant cgroup")
			stdout, stderr, err := e2epod.ExecCommandInContainerWithFullOutput(f, pod.Name, "test", mkdirCmd...)
			framework.ExpectNoError(err, "expected mkdir in /sys/fs/cgroup to succeed; stdout=%q stderr=%q", stdout, stderr)
		})

		ginkgo.It("should mount /sys/fs/cgroup read-only by default", func(ctx context.Context) {
			pod := cgroupOptionsPod("cgroup-readonly", cgroupOptionsContainer("test", nil))
			podClient.CreateSync(ctx, pod)

			ginkgo.By("verifying the container cannot create a descendant cgroup")
			expectCgroupReadOnly(f, pod.Name, "test")
		})

		ginkgo.It("should mount /sys/fs/cgroup read-only when mountMode is ReadOnly", func(ctx context.Context) {
			pod := cgroupOptionsPod("cgroup-explicit-readonly",
				cgroupOptionsContainer("test", ptr.To(v1.CgroupMountModeReadOnly)))
			podClient.CreateSync(ctx, pod)

			ginkgo.By("verifying the container cannot create a descendant cgroup")
			expectCgroupReadOnly(f, pod.Name, "test")
		})

		ginkgo.It("should apply the mount mode per container", func(ctx context.Context) {
			pod := cgroupOptionsPod("cgroup-mixed",
				cgroupOptionsContainer("writable", ptr.To(v1.CgroupMountModeWritable)),
				cgroupOptionsContainer("readonly", ptr.To(v1.CgroupMountModeReadOnly)))
			podClient.CreateSync(ctx, pod)

			ginkgo.By("verifying only the opted-in container can create a descendant cgroup")
			stdout, stderr, err := e2epod.ExecCommandInContainerWithFullOutput(f, pod.Name, "writable", mkdirCmd...)
			framework.ExpectNoError(err, "expected mkdir to succeed in the writable container; stdout=%q stderr=%q", stdout, stderr)

			expectCgroupReadOnly(f, pod.Name, "readonly")
		})

		ginkgo.It("should limit the descendant cgroups a pod with writable cgroups can create", func(ctx context.Context) {
			pod := cgroupOptionsPod("cgroup-descendant-limit",
				cgroupOptionsContainer("test", ptr.To(v1.CgroupMountModeWritable)))
			pod = podClient.CreateSync(ctx, pod)

			ginkgo.By("reading the limits from the pod cgroup")
			cgroupPath := makeCgroupPathForPod(pod, kubeletCfg.CgroupDriver, true)
			descendants, err := os.ReadFile(filepath.Join(cgroupPath, "cgroup.max.descendants"))
			framework.ExpectNoError(err)
			maxDescendants, err := strconv.Atoi(strings.TrimSpace(string(descendants)))
			framework.ExpectNoError(err, "cgroup.max.descendants should be a number, got %q", descendants)
			depth, err := os.ReadFile(filepath.Join(cgroupPath, "cgroup.max.depth"))
			framework.ExpectNoError(err)
			gomega.Expect(strings.TrimSpace(string(depth))).NotTo(gomega.Equal("max"), "the pod cgroup should carry a depth limit")

			ginkgo.By("verifying the container runs out of descendant cgroups")
			// Attempt one more than the Pod limit so this test finishes even if the
			// limit is not enforced.
			created, _, err := e2epod.ExecCommandInContainerWithFullOutput(f, pod.Name, "test",
				"sh", "-c", fmt.Sprintf("i=0; while [ $i -lt %d ] && mkdir /sys/fs/cgroup/d$i 2>/dev/null; do i=$((i+1)); done; echo $i", maxDescendants+1))
			framework.ExpectNoError(err)
			count, err := strconv.Atoi(strings.TrimSpace(created))
			framework.ExpectNoError(err, "expected a count of created cgroups, got %q", created)
			gomega.Expect(count).To(gomega.BeNumerically(">", 0), "the container should be able to create at least one cgroup")
			gomega.Expect(count).To(gomega.BeNumerically("<", maxDescendants),
				"the container should hit the pod's descendant limit")
		})
	})

	ginkgo.Context("when the node does not declare support for writable cgroups", func() {
		ginkgo.BeforeEach(func(ctx context.Context) {
			waitForNodeReady(ctx)
			if nodeDeclaresCgroupOptions(ctx, f) {
				e2eskipper.Skipf("node declares the %s feature", cgroupOptionsFeatureName)
			}
		})

		ginkgo.It("should reject a pod that requests writable cgroups", func(ctx context.Context) {
			pod := cgroupOptionsPod("cgroup-unsupported",
				cgroupOptionsContainer("test", ptr.To(v1.CgroupMountModeWritable)))
			pod = podClient.Create(ctx, pod)

			framework.ExpectNoError(e2epod.WaitForPodFailedReason(ctx, f.ClientSet, pod, lifecycle.PodFeatureUnsupported, framework.PodStartShortTimeout))
		})
	})
})
