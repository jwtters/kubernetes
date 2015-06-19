/*
Copyright 2015 The Kubernetes Authors All rights reserved.

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

package e2e

import (
	"bytes"
	"fmt"
	"net/http"
	"os/exec"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/kubernetes/pkg/api"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/labels"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/util"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/util/wait"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

// version applies to upgrades; kube-push always pushes local binaries.
const version = "latest_ci"

// The following upgrade functions are passed into the framework below and used
// to do the actual upgrades.

var masterUpgrade = func() error {
	_, _, err := runScript("hack/e2e-internal/e2e-upgrade.sh", "-M", version)
	return err
}

var masterPush = func() error {
	_, _, err := runScript("hack/e2e-internal/e2e-push.sh", "-m")
	return err
}

var nodeUpgrade = func(f Framework, replicas int) error {
	Logf("Preparing node upgarde by creating new instance template")
	stdout, _, err := runScript("hack/e2e-internal/e2e-upgrade.sh", "-P", version)
	if err != nil {
		return err
	}
	tmpl := strings.TrimSpace(stdout)

	Logf("Performing a node upgrade to %s; waiting at most %v per node", tmpl, restartPerNodeTimeout)
	if err := migRollingUpdate(tmpl, restartPerNodeTimeout); err != nil {
		return fmt.Errorf("error doing node upgrade via a migRollingUpdate to %s: %v", tmpl, err)
	}

	Logf("Waiting up to %v for all nodes to be ready after the upgrade", restartNodeReadyAgainTimeout)
	if _, err := checkNodesReady(f.Client, restartNodeReadyAgainTimeout, testContext.CloudConfig.NumNodes); err != nil {
		return err
	}

	Logf("Waiting up to %v for all pods to be running and ready after the upgrade", restartPodReadyAgainTimeout)
	return waitForPodsRunningReady(f.Namespace.Name, replicas, restartPodReadyAgainTimeout)
}

var _ = Describe("Skipped", func() {
	Describe("Cluster upgrade", func() {
		svcName, replicas := "baz", 2
		var rcName, ip string
		var ingress api.LoadBalancerIngress
		f := Framework{BaseName: "cluster-upgrade"}
		var w *WebserverTest

		BeforeEach(func() {
			By("Setting up the service, RC, and pods")
			f.beforeEach()
			w = NewWebserverTest(f.Client, f.Namespace.Name, svcName)
			rc := w.CreateWebserverRC(replicas)
			rcName = rc.ObjectMeta.Name
			svc := w.BuildServiceSpec()
			svc.Spec.Type = api.ServiceTypeLoadBalancer
			w.CreateService(svc)

			By("Waiting for the service to become reachable")
			result, err := waitForLoadBalancerIngress(f.Client, svcName, f.Namespace.Name)
			Expect(err).NotTo(HaveOccurred())
			ingresses := result.Status.LoadBalancer.Ingress
			if len(ingresses) != 1 {
				Failf("Was expecting only 1 ingress IP but got %d (%v): %v", len(ingresses), ingresses, result)
			}
			ingress = ingresses[0]
			Logf("Got load balancer ingress point %v", ingress)
			ip = ingress.IP
			if ip == "" {
				ip = ingress.Hostname
			}
			testLoadBalancerReachable(ingress, 80)

			// TODO(mbforbes): Add setup, validate, and teardown for:
			//  - secrets
			//  - volumes
			//  - persistent volumes
		})

		AfterEach(func() {
			f.afterEach()
			w.Cleanup()
		})

		Describe("kube-push", func() {
			It("of master should maintain responsive services", func() {
				By("Validating cluster before master upgrade")
				expectNoError(validate(f, svcName, rcName, ingress, replicas))
				By("Performing a master upgrade")
				testMasterUpgrade(ip, masterPush)
				By("Validating cluster after master upgrade")
				expectNoError(validate(f, svcName, rcName, ingress, replicas))
			})
		})

		Describe("gce-upgrade-master", func() {
			It("should maintain responsive services", func() {
				// TODO(mbforbes): Add GKE support.
				if !providerIs("gce") {
					By(fmt.Sprintf("Skipping upgrade test, which is not implemented for %s", testContext.Provider))
					return
				}
				By("Validating cluster before master upgrade")
				expectNoError(validate(f, svcName, rcName, ingress, replicas))
				By("Performing a master upgrade")
				testMasterUpgrade(ip, masterUpgrade)
				By("Validating cluster after master upgrade")
				expectNoError(validate(f, svcName, rcName, ingress, replicas))
			})
		})

		Describe("gce-upgrade-cluster", func() {
			var tmplBefore, tmplAfter string
			BeforeEach(func() {
				By("Getting the node template before the upgrade")
				var err error
				tmplBefore, err = migTemplate()
				expectNoError(err)
			})

			AfterEach(func() {
				By("Cleaning up any unused node templates")
				var err error
				tmplAfter, err = migTemplate()
				if err != nil {
					Logf("Could not get node template post-upgrade; may have leaked template %s", tmplBefore)
					return
				}
				if tmplBefore == tmplAfter {
					// The node upgrade failed so there's no need to delete
					// anything.
					Logf("Node template %s is still in use; not cleaning up", tmplBefore)
					return
				}
				// TODO(mbforbes): Distinguish between transient failures
				// and "cannot delete--in use" errors and retry on the
				// former.
				Logf("Deleting node template %s", tmplBefore)
				o, err := exec.Command("gcloud", "compute", "instance-templates",
					fmt.Sprintf("--project=%s", testContext.CloudConfig.ProjectID),
					"delete",
					tmplBefore).CombinedOutput()
				if err != nil {
					Logf("gcloud compute instance-templates delete %s call failed with err: %v, output: %s",
						tmplBefore, err, string(o))
					Logf("May have leaked %s", tmplBefore)
				}
			})

			It("should maintain a functioning cluster", func() {
				// TODO(mbforbes): Add GKE support.
				if !providerIs("gce") {
					By(fmt.Sprintf("Skipping upgrade test, which is not implemented for %s", testContext.Provider))
					return
				}
				By("Validating cluster before master upgrade")
				expectNoError(validate(f, svcName, rcName, ingress, replicas))
				By("Performing a master upgrade")
				testMasterUpgrade(ip, masterUpgrade)
				By("Validating cluster after master upgrade")
				expectNoError(validate(f, svcName, rcName, ingress, replicas))
				By("Performing a node upgrade")
				testNodeUpgrade(f, nodeUpgrade, replicas)
				By("Validating cluster after node upgrade")
				expectNoError(validate(f, svcName, rcName, ingress, replicas))
			})
		})
	})
})

func testMasterUpgrade(ip string, mUp func() error) {
	Logf("Starting async validation")
	httpClient := http.Client{Timeout: 2 * time.Second}
	done := make(chan struct{}, 1)
	// Let's make sure we've finished the heartbeat before shutting things down.
	var wg sync.WaitGroup
	go util.Until(func() {
		defer GinkgoRecover()
		wg.Add(1)
		defer wg.Done()

		if err := wait.Poll(poll, singleCallTimeout, func() (bool, error) {
			r, err := httpClient.Get("http://" + ip)
			if err != nil {
				Logf("Error reaching %s: %v", ip, err)
				return false, nil
			}
			if r.StatusCode < http.StatusOK || r.StatusCode >= http.StatusNotFound {
				Logf("Bad response; status: %d, response: %v", r.StatusCode, r)
				return false, nil
			}
			return true, nil
		}); err != nil {
			// We log the error here because the test will fail at the very end
			// because this validation runs in another goroutine. Without this,
			// a failure is very confusing to track down because from the logs
			// everything looks fine.
			msg := fmt.Sprintf("Failed to contact service during master upgrade: %v", err)
			Logf(msg)
			Failf(msg)
		}
	}, 200*time.Millisecond, done)

	Logf("Starting master upgrade")
	expectNoError(mUp())
	done <- struct{}{}
	Logf("Stopping async validation")
	wg.Wait()
	Logf("Master upgrade complete")
}

func testNodeUpgrade(f Framework, nUp func(f Framework, n int) error, replicas int) {
	Logf("Starting node upgrade")
	expectNoError(nUp(f, replicas))
	Logf("Node upgrade complete")

	// TODO(mbforbes): Validate that:
	// - the node software version truly changed

}

// runScript runs script on testContext.RepoRoot using args and returns
// stdout, stderr, and error.
func runScript(script string, args ...string) (string, string, error) {
	Logf("Running %s %v", script, args)
	var bout, berr bytes.Buffer
	cmd := exec.Command(path.Join(testContext.RepoRoot, script), args...)
	cmd.Stdout, cmd.Stderr = &bout, &berr
	err := cmd.Run()
	stdout, stderr := bout.String(), berr.String()
	if err != nil {
		return "", "", fmt.Errorf("error running %s %v; got error %v, stdout %q, stderr %q",
			script, args, err, stdout, stderr)
	}
	Logf("stdout: %s", stdout)
	Logf("stderr: %s", stderr)
	return stdout, stderr, nil
}

func validate(f Framework, svcNameWant, rcNameWant string, ingress api.LoadBalancerIngress, podsWant int) error {
	Logf("Beginning cluster validation")
	// Verify RC.
	rcs, err := f.Client.ReplicationControllers(f.Namespace.Name).List(labels.Everything())
	if err != nil {
		return fmt.Errorf("error listing RCs: %v", err)
	}
	if len(rcs.Items) != 1 {
		return fmt.Errorf("wanted 1 RC with name %s, got %d", rcNameWant, len(rcs.Items))
	}
	if got := rcs.Items[0].Name; got != rcNameWant {
		return fmt.Errorf("wanted RC name %q, got %q", rcNameWant, got)
	}

	// Verify pods.
	if err := verifyPods(f.Client, f.Namespace.Name, rcNameWant, false, podsWant); err != nil {
		return fmt.Errorf("failed to find %d %q pods: %v", podsWant, rcNameWant, err)
	}

	// Verify service.
	svc, err := f.Client.Services(f.Namespace.Name).Get(svcNameWant)
	if err != nil {
		return fmt.Errorf("error getting service %s: %v", svcNameWant, err)
	}
	if svcNameWant != svc.Name {
		return fmt.Errorf("wanted service name %q, got %q", svcNameWant, svc.Name)
	}
	// TODO(mbforbes): Make testLoadBalancerReachable return an error.
	testLoadBalancerReachable(ingress, 80)

	Logf("Cluster validation succeeded")
	return nil
}
