/*
Copyright 2014 The Kubernetes Authors.

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

package main

import (
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Add more default --test_args as we migrate them
func argFields(args, dump, ipRange string) []string {
	f := strings.Fields(args)
	if dump != "" {
		f = setFieldDefault(f, "--report-dir", dump)
	}
	if ipRange != "" {
		f = setFieldDefault(f, "--cluster-ip-range", ipRange)
	}
	return f
}

func run(deploy deployer, o options) error {
	if o.checkSkew {
		os.Setenv("KUBECTL", "./cluster/kubectl.sh --match-server-version")
	} else {
		os.Setenv("KUBECTL", "./cluster/kubectl.sh")
	}
	os.Setenv("KUBE_CONFIG_FILE", "config-test.sh")
	os.Setenv("KUBE_RUNTIME_CONFIG", o.runtimeConfig)

	dump := o.dump
	if dump != "" {
		if !filepath.IsAbs(dump) { // Directory may change
			wd, err := os.Getwd()
			if err != nil {
				return fmt.Errorf("failed to os.Getwd(): %v", err)
			}
			dump = filepath.Join(wd, dump)
		}
	}

	if o.up {
		if o.federation {
			if err := xmlWrap("Federation TearDown Previous", fedDown); err != nil {
				return fmt.Errorf("error tearing down previous federation control plane: %v", err)
			}
		}
		if err := xmlWrap("TearDown Previous", deploy.Down); err != nil {
			return fmt.Errorf("error tearing down previous cluster: %s", err)
		}
	}

	var err error
	var errs []error

	// Ensures that the cleanup/down action is performed exactly once.
	var (
		downDone           = false
		federationDownDone = false
	)

	var (
		beforeResources []byte
		upResources     []byte
		downResources   []byte
		afterResources  []byte
	)

	if o.checkLeaks {
		errs = appendError(errs, xmlWrap("listResources Before", func() error {
			beforeResources, err = listResources()
			return err
		}))
	}

	if o.up {
		// If we tried to bring the cluster up, make a courtesy
		// attempt to bring it down so we're not leaving resources around.
		if o.down {
			defer xmlWrap("Deferred TearDown", func() error {
				if !downDone {
					return deploy.Down()
				}
				return nil
			})
			// Deferred statements are executed in last-in-first-out order, so
			// federation down defer must appear after the cluster teardown in
			// order to execute that before cluster teardown.
			if o.federation {
				defer xmlWrap("Deferred Federation TearDown", func() error {
					if !federationDownDone {
						return fedDown()
					}
					return nil
				})
			}
		}
		// Start the cluster using this version.
		if err := xmlWrap("Up", deploy.Up); err != nil {
			if dump != "" {
				xmlWrap("DumpClusterLogs (--up failed)", func() error {
					// This frequently means the cluster does not exist.
					// Thus DumpClusterLogs() typically fails.
					// Therefore always return null for this scenarios.
					// TODO(fejta): report a green E in testgrid if it errors.
					deploy.DumpClusterLogs(dump, o.logexporterGCSPath)
					return nil
				})
			}
			return fmt.Errorf("starting e2e cluster: %s", err)
		}
		if o.federation {
			if err := xmlWrap("Federation Up", fedUp); err != nil {
				xmlWrap("dumpFederationLogs", func() error {
					return dumpFederationLogs(dump)
				})
				return fmt.Errorf("error starting federation: %s", err)
			}
		}
		// Check that the api is reachable before proceeding with further steps.
		errs = appendError(errs, xmlWrap("Check APIReachability", getKubectlVersion))
		if dump != "" {
			errs = appendError(errs, xmlWrap("list nodes", func() error {
				return listNodes(dump)
			}))
		}
	}

	if o.checkLeaks {
		errs = appendError(errs, xmlWrap("listResources Up", func() error {
			upResources, err = listResources()
			return err
		}))
	}

	if o.upgradeArgs != "" {
		errs = appendError(errs, xmlWrap("UpgradeTest", func() error {
			return skewTest(argFields(o.upgradeArgs, dump, o.clusterIPRange), "upgrade", o.checkSkew)
		}))
	}

	testArgs := argFields(o.testArgs, dump, o.clusterIPRange)
	if o.test {
		if err := xmlWrap("test setup", deploy.TestSetup); err != nil {
			errs = appendError(errs, err)
		} else {
			errs = appendError(errs, xmlWrap("kubectl version", getKubectlVersion))
			if o.skew {
				errs = appendError(errs, xmlWrap("SkewTest", func() error {
					return skewTest(testArgs, "skew", o.checkSkew)
				}))
			} else {
				if err := xmlWrap("IsUp", deploy.IsUp); err != nil {
					errs = appendError(errs, err)
				} else {
					if o.federation {
						errs = appendError(errs, xmlWrap("FederationTest", func() error {
							return federationTest(testArgs)
						}))
					} else {
						errs = appendError(errs, xmlWrap("Test", func() error {
							return test(testArgs)
						}))
					}
				}
			}
		}
	}

	if o.kubemark {
		errs = appendError(errs, xmlWrap("Kubemark", func() error {
			return kubemarkTest(testArgs, dump, o.kubemarkNodes, o.kubemarkMasterSize, o.clusterIPRange)
		}))
	}

	if o.charts {
		errs = appendError(errs, xmlWrap("Helm Charts", chartsTest))
	}

	if o.perfTests {
		errs = appendError(errs, xmlWrap("Perf Tests", perfTest))
	}

	if len(errs) > 0 && dump != "" {
		errs = appendError(errs, xmlWrap("DumpClusterLogs", func() error {
			return deploy.DumpClusterLogs(dump, o.logexporterGCSPath)
		}))
		if o.federation {
			errs = appendError(errs, xmlWrap("dumpFederationLogs", func() error {
				return dumpFederationLogs(dump)
			}))
		}
	}

	if o.checkLeaks {
		errs = appendError(errs, xmlWrap("listResources Down", func() error {
			downResources, err = listResources()
			return err
		}))
	}

	if o.down {
		if o.federation {
			errs = appendError(errs, xmlWrap("Federation TearDown", func() error {
				if !federationDownDone {
					err := fedDown()
					if err != nil {
						return err
					}
					federationDownDone = true
				}
				return nil
			}))
		}
		errs = appendError(errs, xmlWrap("TearDown", func() error {
			if !downDone {
				err := deploy.Down()
				if err != nil {
					return err
				}
				downDone = true
			}
			return nil
		}))
	}

	if o.checkLeaks {
		log.Print("Sleeping for 30 seconds...") // Wait for eventually consistent listing
		time.Sleep(30 * time.Second)
		if err := xmlWrap("listResources After", func() error {
			afterResources, err = listResources()
			return err
		}); err != nil {
			errs = append(errs, err)
		} else {
			errs = appendError(errs, xmlWrap("diffResources", func() error {
				return diffResources(beforeResources, upResources, downResources, afterResources, dump)
			}))
		}
	}
	if len(errs) == 0 && o.publish != "" {
		errs = appendError(errs, xmlWrap("Publish version", func() error {
			// Use plaintext version file packaged with kubernetes.tar.gz
			v, err := ioutil.ReadFile("version")
			if err != nil {
				return err
			}
			log.Printf("Set %s version to %s", o.publish, string(v))
			return finishRunning(exec.Command("gsutil", "cp", "version", o.publish))
		}))
	}

	if len(errs) != 0 {
		return fmt.Errorf("encountered %d errors: %v", len(errs), errs)
	}
	return nil
}

func getKubectlVersion() error {
	retries := 5
	for {
		_, err := output(exec.Command("./cluster/kubectl.sh", "--match-server-version=false", "version"))
		if err == nil {
			return nil
		}
		retries--
		if retries == 0 {
			return err
		}
		log.Print("Failed to reach api. Sleeping for 10 seconds before retrying...")
		time.Sleep(10 * time.Second)
	}
}

func listNodes(dump string) error {
	b, err := output(exec.Command("./cluster/kubectl.sh", "--match-server-version=false", "get", "nodes", "-oyaml"))
	if err != nil {
		return err
	}
	return ioutil.WriteFile(filepath.Join(dump, "nodes.yaml"), b, 0644)
}

func diffResources(before, clusterUp, clusterDown, after []byte, location string) error {
	if location == "" {
		var err error
		location, err = ioutil.TempDir("", "e2e-check-resources")
		if err != nil {
			return fmt.Errorf("Could not create e2e-check-resources temp dir: %s", err)
		}
	}

	var mode os.FileMode = 0664
	bp := filepath.Join(location, "gcp-resources-before.txt")
	up := filepath.Join(location, "gcp-resources-cluster-up.txt")
	cdp := filepath.Join(location, "gcp-resources-cluster-down.txt")
	ap := filepath.Join(location, "gcp-resources-after.txt")
	dp := filepath.Join(location, "gcp-resources-diff.txt")

	if err := ioutil.WriteFile(bp, before, mode); err != nil {
		return err
	}
	if err := ioutil.WriteFile(up, clusterUp, mode); err != nil {
		return err
	}
	if err := ioutil.WriteFile(cdp, clusterDown, mode); err != nil {
		return err
	}
	if err := ioutil.WriteFile(ap, after, mode); err != nil {
		return err
	}

	stdout, cerr := output(exec.Command("diff", "-sw", "-U0", "-F^\\[.*\\]$", bp, ap))
	if err := ioutil.WriteFile(dp, stdout, mode); err != nil {
		return err
	}
	if cerr == nil { // No diffs
		return nil
	}
	lines := strings.Split(string(stdout), "\n")
	if len(lines) < 3 { // Ignore the +++ and --- header lines
		return nil
	}
	lines = lines[2:]

	var added, report []string
	resourceTypeRE := regexp.MustCompile(`^@@.+\s(\[\s\S+\s\])$`)
	for _, l := range lines {
		if matches := resourceTypeRE.FindStringSubmatch(l); matches != nil {
			report = append(report, matches[1])
		}
		if strings.HasPrefix(l, "+") && len(strings.TrimPrefix(l, "+")) > 0 {
			added = append(added, l)
			report = append(report, l)
		}
	}
	if len(added) > 0 {
		return fmt.Errorf("Error: %d leaked resources\n%v", len(added), strings.Join(report, "\n"))
	}
	return nil
}

func listResources() ([]byte, error) {
	log.Printf("Listing resources...")
	stdout, err := output(exec.Command("./cluster/gce/list-resources.sh"))
	if err != nil {
		return stdout, fmt.Errorf("Failed to list resources (%s):\n%s", err, string(stdout))
	}
	return stdout, err
}

func clusterSize(deploy deployer) (int, error) {
	if err := deploy.TestSetup(); err != nil {
		return -1, err
	}
	o, err := output(exec.Command("kubectl", "get", "nodes", "--no-headers"))
	if err != nil {
		log.Printf("kubectl get nodes failed: %s\n%s", wrapError(err).Error(), string(o))
		return -1, err
	}
	stdout := strings.TrimSpace(string(o))
	log.Printf("Cluster nodes:\n%s", stdout)
	return len(strings.Split(stdout, "\n")), nil
}

// commandError will provide stderr output (if available) from structured
// exit errors
type commandError struct {
	err error
}

func wrapError(err error) *commandError {
	if err == nil {
		return nil
	}
	return &commandError{err: err}
}

func (e *commandError) Error() string {
	if e == nil {
		return ""
	}
	exitErr, ok := e.err.(*exec.ExitError)
	if !ok {
		return e.err.Error()
	}

	stderr := ""
	if exitErr.Stderr != nil {
		stderr = string(stderr)
	}
	return fmt.Sprintf("%q: %q", exitErr.Error(), stderr)
}

func isUp(d deployer) error {
	n, err := clusterSize(d)
	if err != nil {
		return err
	}
	if n <= 0 {
		return fmt.Errorf("cluster found, but %d nodes reported", n)
	}
	return nil
}

func waitForNodes(d deployer, nodes int, timeout time.Duration) error {
	for stop := time.Now().Add(timeout); time.Now().Before(stop); time.Sleep(30 * time.Second) {
		n, err := clusterSize(d)
		if err != nil {
			log.Printf("Can't get cluster size, sleeping: %v", err)
			continue
		}
		if n < nodes {
			log.Printf("%d (current nodes) < %d (requested instances), sleeping", n, nodes)
			continue
		}
		return nil
	}
	return fmt.Errorf("waiting for nodes timed out")
}

func defaultDumpClusterLogs(localArtifactsDir, logexporterGCSPath string) error {
	var cmd *exec.Cmd
	if logexporterGCSPath != "" {
		log.Printf("Dumping logs from nodes to GCS directly at path: %v", logexporterGCSPath)
		cmd = exec.Command("./cluster/log-dump/log-dump.sh", localArtifactsDir, logexporterGCSPath)
	} else {
		log.Printf("Dumping logs locally to: %v", localArtifactsDir)
		cmd = exec.Command("./cluster/log-dump/log-dump.sh", localArtifactsDir)
	}
	return finishRunning(cmd)
}

func dumpFederationLogs(location string) error {
	logDumpPath := "./federation/cluster/log-dump.sh"
	// federation/cluster/log-dump.sh only exists in the Kubernetes tree
	// post-1.6. If it doesn't exist, do nothing and do not report an error.
	if _, err := os.Stat(logDumpPath); err == nil {
		log.Printf("Dumping Federation logs to: %v", location)
		return finishRunning(exec.Command(logDumpPath, location))
	}
	log.Printf("Could not find %s. This is expected if running tests against a Kubernetes 1.6 or older tree.", logDumpPath)
	return nil
}

func perfTest() error {
	// Run perf tests
	// TODO(fejta): GOPATH may be split by :
	cmdline := fmt.Sprintf("%s/src/k8s.io/perf-tests/clusterloader/run-e2e.sh", os.Getenv("GOPATH"))
	if err := finishRunning(exec.Command(cmdline)); err != nil {
		return err
	}
	return nil
}

func chartsTest() error {
	// Run helm tests.
	if err := finishRunning(exec.Command("/src/k8s.io/charts/test/helm-test-e2e.sh")); err != nil {
		return err
	}
	return nil
}

func kubemarkTest(testArgs []string, dump, nodes, masterSize, ipRange string) error {
	if err := migrateOptions([]migratedOption{
		{
			env:      "KUBEMARK_NUM_NODES",
			option:   &nodes,
			name:     "--kubemark-nodes",
			skipPush: true,
		},
		{
			env:      "KUBEMARK_MASTER_SIZE",
			option:   &masterSize,
			name:     "--kubemark-master-size",
			skipPush: true,
		},
	}); err != nil {
		return err
	}

	// Stop previous run
	err := finishRunning(exec.Command("./test/kubemark/stop-kubemark.sh"))
	if err != nil {
		return err
	}
	// If we tried to bring the Kubemark cluster up, make a courtesy
	// attempt to bring it down so we're not leaving resources around.
	//
	// TODO: We should try calling stop-kubemark exactly once. Though to
	// stop the leaking resources for now, we want to be on the safe side
	// and call it explicitly in defer if the other one is not called.
	defer xmlWrap("Deferred Stop kubemark", func() error {
		return finishRunning(exec.Command("./test/kubemark/stop-kubemark.sh"))
	})

	// Start new run
	popN, err := pushEnv("NUM_NODES", nodes)
	if err != nil {
		return err
	}
	defer popN()
	popM, err := pushEnv("MASTER_SIZE", masterSize)
	if err != nil {
		return err
	}
	defer popM()
	popC, err := pushEnv("CLUSTER_IP_RANGE", ipRange)
	if err != nil {
		return err
	}
	defer popC()

	err = xmlWrap("Start kubemark", func() error {
		return finishRunning(exec.Command("./test/kubemark/start-kubemark.sh"))
	})
	if err != nil {
		if dump != "" {
			log.Printf("Start kubemark step failed, trying to dump logs from kubemark master...")
			if logErr := finishRunning(exec.Command("./test/kubemark/master-log-dump.sh", dump)); logErr != nil {
				// This can happen in case of non-SSH'able kubemark master.
				log.Printf("Failed to dump logs from kubemark master: %v", logErr)
			}
		}
		return err
	}

	if err := os.Unsetenv("CLUSTER_IP_RANGE"); err != nil { // Handled by --cluster-ip-range
		return err
	}
	// Run kubemark tests
	testArgs = setFieldDefault(testArgs, "--ginkgo.focus", "starting\\s30\\pods")

	err = finishRunning(exec.Command("./test/kubemark/run-e2e-tests.sh", testArgs...))
	if err != nil {
		return err
	}

	err = xmlWrap("Stop kubemark", func() error {
		return finishRunning(exec.Command("./test/kubemark/stop-kubemark.sh"))
	})
	if err != nil {
		return err
	}
	return nil
}

// Runs tests in the kubernetes_skew directory, appending --repor-prefix flag to the run
func skewTest(args []string, prefix string, checkSkew bool) error {
	// TODO(fejta): run this inside this kubetest process, do not spawn a new one.
	popS, err := pushd("../kubernetes_skew")
	if err != nil {
		return err
	}
	defer popS()
	args = appendField(args, "--report-prefix", prefix)
	return finishRunning(exec.Command(
		kubetestPath,
		"--test",
		"--test_args="+strings.Join(args, " "),
		fmt.Sprintf("--v=%t", verbose),
		fmt.Sprintf("--check-version-skew=%t", checkSkew),
	))
}

func test(testArgs []string) error {
	return finishRunning(exec.Command("./hack/ginkgo-e2e.sh", testArgs...))
}
