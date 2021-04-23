/*
Copyright 2020 The Kubernetes Authors.

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

// This suite tests volumes under stress conditions

package testsuites

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/onsi/ginkgo"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/kubernetes/test/e2e/framework"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
	e2epv "k8s.io/kubernetes/test/e2e/framework/pv"
	e2eskipper "k8s.io/kubernetes/test/e2e/framework/skipper"
	storageframework "k8s.io/kubernetes/test/e2e/storage/framework"
)

type volumeStressTestSuite struct {
	tsInfo storageframework.TestSuiteInfo
}

type volumeStressTest struct {
	config        *storageframework.PerTestConfig
	driverCleanup func()

	migrationCheck *migrationOpCheck

	volumes []*storageframework.VolumeResource
	pods    []*v1.Pod
	// stop and wait for any async routines
	wg     sync.WaitGroup
	ctx    context.Context
	cancel context.CancelFunc

	testOptions storageframework.StressTestOptions
}

var _ storageframework.TestSuite = &volumeStressTestSuite{}

// InitCustomVolumeStressTestSuite returns volumeStressTestSuite that implements TestSuite interface
// using custom test patterns
func InitCustomVolumeStressTestSuite(patterns []storageframework.TestPattern) storageframework.TestSuite {
	return &volumeStressTestSuite{
		tsInfo: storageframework.TestSuiteInfo{
			Name:         "volume-stress",
			TestPatterns: patterns,
		},
	}
}

// InitVolumeStressTestSuite returns volumeStressTestSuite that implements TestSuite interface
// using testsuite default patterns
func InitVolumeStressTestSuite() storageframework.TestSuite {
	patterns := []storageframework.TestPattern{
		storageframework.DefaultFsDynamicPV,
		storageframework.BlockVolModeDynamicPV,
	}
	return InitCustomVolumeStressTestSuite(patterns)
}

func (t *volumeStressTestSuite) GetTestSuiteInfo() storageframework.TestSuiteInfo {
	return t.tsInfo
}

func (t *volumeStressTestSuite) SkipUnsupportedTests(driver storageframework.TestDriver, pattern storageframework.TestPattern) {
	dInfo := driver.GetDriverInfo()
	if dInfo.StressTestOptions == nil {
		e2eskipper.Skipf("Driver %s doesn't specify stress test options -- skipping", dInfo.Name)
	}
	if dInfo.StressTestOptions.NumPods <= 0 {
		framework.Failf("NumPods in stress test options must be a positive integer, received: %d", dInfo.StressTestOptions.NumPods)
	}
	if dInfo.StressTestOptions.NumRestarts <= 0 {
		framework.Failf("NumRestarts in stress test options must be a positive integer, received: %d", dInfo.StressTestOptions.NumRestarts)
	}

	if _, ok := driver.(storageframework.DynamicPVTestDriver); !ok {
		e2eskipper.Skipf("Driver %s doesn't implement DynamicPVTestDriver -- skipping", dInfo.Name)
	}
	if !driver.GetDriverInfo().Capabilities[storageframework.CapBlock] && pattern.VolMode == v1.PersistentVolumeBlock {
		e2eskipper.Skipf("Driver %q does not support block volume mode - skipping", dInfo.Name)
	}
}

func (t *volumeStressTestSuite) DefineTests(driver storageframework.TestDriver, pattern storageframework.TestPattern) {
	var (
		dInfo = driver.GetDriverInfo()
		cs    clientset.Interface
		l     *volumeStressTest
	)

	// Beware that it also registers an AfterEach which renders f unusable. Any code using
	// f must run inside an It or Context callback.
	f := framework.NewFrameworkWithCustomTimeouts("stress", storageframework.GetDriverTimeouts(driver))

	init := func() {
		cs = f.ClientSet
		l = &volumeStressTest{}

		// Now do the more expensive test initialization.
		l.config, l.driverCleanup = driver.PrepareTest(f)
		l.migrationCheck = newMigrationOpCheck(f.ClientSet, dInfo.InTreePluginName)
		l.volumes = []*storageframework.VolumeResource{}
		l.pods = []*v1.Pod{}
		l.testOptions = *dInfo.StressTestOptions
		l.ctx, l.cancel = context.WithCancel(context.Background())
	}

	createPodsAndVolumes := func() {
		scName := "standard-rwx-retain"
		pvc := v1.PersistentVolumeClaim{
			Spec: v1.PersistentVolumeClaimSpec{
				AccessModes: []v1.PersistentVolumeAccessMode{
					v1.ReadWriteMany,
				},
				StorageClassName: &scName,
				Resources: v1.ResourceRequirements{
					Requests: v1.ResourceList{
						v1.ResourceStorage: resource.MustParse("100Gi"),
					},
				},
				VolumeName: "pvc-599dcb7d-c32a-4fbe-b40c-594668b6ea0f",
			},
		}
		pvc.Name = "test-pvc"
		pvc.Namespace = f.Namespace.Name

		if _, err := cs.CoreV1().PersistentVolumeClaims(pvc.Namespace).Create(context.TODO(), &pvc, metav1.CreateOptions{}); err != nil {
			framework.Failf("Failed to create pvc [%+v]. Error: %v", pvc.Name, err)
		}

		for i := 0; i < l.testOptions.NumPods; i++ {
			framework.Logf("Creating resources for pod %v/%v", i, l.testOptions.NumPods-1)
			podConfig := e2epod.Config{
				NS:           f.Namespace.Name,
				PVCs:         []*v1.PersistentVolumeClaim{&pvc},
				SeLinuxLabel: e2epv.SELinuxLabel,
			}
			pod, err := e2epod.MakeSecPod(&podConfig)
			framework.ExpectNoError(err)

			l.pods = append(l.pods, pod)
		}
	}

	ginkgo.BeforeEach(func() {
		init()
		createPodsAndVolumes()
	})

	var (
		startTimes = []time.Duration{}
		mu         sync.Mutex
	)

	reportStartTimes := func() {
		for _, time := range startTimes {
			fmt.Println(time)
		}
	}

	// See #96177, this is necessary for cleaning up resources when tests are interrupted.
	f.AddAfterEach("cleanup", func(f *framework.Framework, failed bool) {
		reportStartTimes()
	})

	ginkgo.It("multiple pods should access different volumes repeatedly [Slow] [Serial]", func() {
		// Restart pod repeatedly
		for i := 0; i < l.testOptions.NumPods; i++ {
			podIndex := i
			l.wg.Add(1)
			go func() {
				defer ginkgo.GinkgoRecover()
				defer l.wg.Done()
				for j := 0; j < l.testOptions.NumRestarts; j++ {
					select {
					case <-l.ctx.Done():
						return
					default:
						pod := l.pods[podIndex]
						framework.Logf("Pod-%v [%v], Iteration %v/%v", podIndex, pod.Name, j, l.testOptions.NumRestarts-1)

						now := time.Now()
						_, err := cs.CoreV1().Pods(pod.Namespace).Create(context.TODO(), pod, metav1.CreateOptions{})
						if err != nil {
							framework.Logf("CHRIS: Failed to create pod-%v [%+v]. Error: %v", podIndex, pod, err)
							continue
						}

						err = e2epod.WaitTimeoutForPodRunningInNamespace(cs, pod.Name, pod.Namespace, f.Timeouts.PodStartSlow)
						if err != nil {
							l.cancel()
							framework.Failf("Failed to wait for pod-%v [%+v] turn into running status. Error: %v", podIndex, pod, err)
						}

						mu.Lock()
						startTimes = append(startTimes, time.Since(now))
						mu.Unlock()

						// TODO: write data per pod and validate it everytime

						err = e2epod.DeletePodWithWait(f.ClientSet, pod)
						if err != nil {
							l.cancel()
							framework.Failf("Failed to delete pod-%v [%+v]. Error: %v", podIndex, pod, err)
						}
					}
				}
			}()
		}

		l.wg.Wait()

		// TODO: Bucket this once you get some general times.
		sort.Slice(startTimes, func(i, j int) bool {
			return startTimes[i] < startTimes[j]
		})
	})
}
