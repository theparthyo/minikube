/*
Copyright 2016 The Kubernetes Authors All rights reserved.

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

package cmd

import (
	"bufio"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"time"

	"github.com/docker/machine/libmachine/mcnerror"
	"github.com/golang/glog"
	"github.com/pkg/browser"
	"github.com/pkg/errors"
	"github.com/spf13/cobra"
	configcmd "k8s.io/minikube/cmd/minikube/cmd/config"
	"k8s.io/minikube/pkg/minikube/assets"
	"k8s.io/minikube/pkg/minikube/cluster"
	pkg_config "k8s.io/minikube/pkg/minikube/config"
	"k8s.io/minikube/pkg/minikube/exit"
	"k8s.io/minikube/pkg/minikube/machine"
	"k8s.io/minikube/pkg/minikube/out"
	"k8s.io/minikube/pkg/minikube/proxy"
	"k8s.io/minikube/pkg/minikube/service"
	"k8s.io/minikube/pkg/util"
)

var (
	dashboardURLMode bool
	// Matches: 127.0.0.1:8001
	// TODO(tstromberg): Get kubectl to implement a stable supported output format.
	hostPortRe = regexp.MustCompile(`127.0.0.1:\d{4,}`)
)

// dashboardCmd represents the dashboard command
var dashboardCmd = &cobra.Command{
	Use:   "dashboard",
	Short: "Access the kubernetes dashboard running within the minikube cluster",
	Long:  `Access the kubernetes dashboard running within the minikube cluster`,
	Run: func(cmd *cobra.Command, args []string) {
		cc, err := pkg_config.Load()
		if err != nil && !os.IsNotExist(err) {
			exit.WithError("Error loading profile config", err)
		}

		api, err := machine.NewAPIClient()
		defer func() {
			err := api.Close()
			if err != nil {
				glog.Warningf("Failed to close API: %v", err)
			}
		}()

		if err != nil {
			exit.WithError("Error getting client", err)
		}

		if _, err = api.Load(pkg_config.GetMachineName()); err != nil {
			switch err := errors.Cause(err).(type) {
			case mcnerror.ErrHostDoesNotExist:
				exit.WithCodeT(exit.Unavailable, "{{.name}} cluster does not exist", out.V{"name": pkg_config.GetMachineName()})
			default:
				exit.WithError("Error getting cluster", err)
			}
		}

		err = proxy.ExcludeIP(cc.KubernetesConfig.NodeIP) // to be used for http get calls
		if err != nil {
			glog.Errorf("Error excluding IP from proxy: %s", err)
		}

		kubectl, err := exec.LookPath("kubectl")
		if err != nil {
			exit.WithCodeT(exit.NoInput, "kubectl not found in PATH, but is required for the dashboard. Installation guide: https://kubernetes.io/docs/tasks/tools/install-kubectl/")
		}

		cluster.EnsureMinikubeRunningOrExit(api, 1)

		// Check dashboard status before enabling it
		dashboardAddon := assets.Addons["dashboard"]
		dashboardStatus, _ := dashboardAddon.IsEnabled()
		if !dashboardStatus {
			// Send status messages to stderr for folks re-using this output.
			out.ErrT(out.Enabling, "Enabling dashboard ...")
			// Enable the dashboard add-on
			err = configcmd.Set("dashboard", "true")
			if err != nil {
				exit.WithError("Unable to enable dashboard", err)
			}
		}

		ns := "kube-system"
		svc := "kubernetes-dashboard"
		out.ErrT(out.Verifying, "Verifying dashboard health ...")
		if err = util.RetryAfter(180, func() error { return service.CheckService(ns, svc) }, 1*time.Second); err != nil {
			exit.WithCodeT(exit.Unavailable, "dashboard service is not running: {{.error}}", out.V{"error": err})
		}

		out.ErrT(out.Launch, "Launching proxy ...")
		p, hostPort, err := kubectlProxy(kubectl)
		if err != nil {
			exit.WithError("kubectl proxy", err)
		}
		url := dashboardURL(hostPort, ns, svc)

		out.ErrT(out.Verifying, "Verifying proxy health ...")
		if err = util.RetryAfter(60, func() error { return checkURL(url) }, 1*time.Second); err != nil {
			exit.WithCodeT(exit.Unavailable, "{{.url}} is not accessible: {{.error}}", out.V{"url": url, "error": err})
		}

		if dashboardURLMode {
			out.Ln(url)
		} else {
			out.ErrT(out.Celebrate, "Opening %s in your default browser...", out.V{"url": url})
			if err = browser.OpenURL(url); err != nil {
				exit.WithCodeT(exit.Software, "failed to open browser: {{.error}}", out.V{"error": err})
			}
		}

		glog.Infof("Success! I will now quietly sit around until kubectl proxy exits!")
		if err = p.Wait(); err != nil {
			glog.Errorf("Wait: %v", err)
		}
	},
}

// kubectlProxy runs "kubectl proxy", returning host:port
func kubectlProxy(path string) (*exec.Cmd, string, error) {
	// port=0 picks a random system port
	// pkg_config.GetMachineName() respects the -p (profile) flag

	cmd := exec.Command(path, "--context", pkg_config.GetMachineName(), "proxy", "--port=0")

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return nil, "", errors.Wrap(err, "cmd stdout")
	}

	glog.Infof("Executing: %s %s", cmd.Path, cmd.Args)
	if err := cmd.Start(); err != nil {
		return nil, "", errors.Wrap(err, "proxy start")
	}

	glog.Infof("Waiting for kubectl to output host:port ...")
	reader := bufio.NewReader(stdoutPipe)

	var out []byte
	for {
		r, timedOut, err := readByteWithTimeout(reader, 5*time.Second)
		if err != nil {
			return cmd, "", fmt.Errorf("readByteWithTimeout: %v", err)
		}
		if r == byte('\n') {
			break
		}
		if timedOut {
			glog.Infof("timed out waiting for input: possibly due to an old kubectl version.")
			break
		}
		out = append(out, r)
	}
	glog.Infof("proxy stdout: %s", string(out))
	return cmd, hostPortRe.FindString(string(out)), nil
}

// readByteWithTimeout returns a byte from a reader or an indicator that a timeout has occurred.
func readByteWithTimeout(r io.ByteReader, timeout time.Duration) (byte, bool, error) {
	bc := make(chan byte)
	ec := make(chan error)
	go func() {
		b, err := r.ReadByte()
		if err != nil {
			ec <- err
		} else {
			bc <- b
		}
		close(bc)
		close(ec)
	}()
	select {
	case b := <-bc:
		return b, false, nil
	case err := <-ec:
		return byte(' '), false, err
	case <-time.After(timeout):
		return byte(' '), true, nil
	}
}

// dashboardURL generates a URL for accessing the dashboard service
func dashboardURL(proxy string, ns string, svc string) string {
	// Reference: https://github.com/kubernetes/dashboard/wiki/Accessing-Dashboard---1.7.X-and-above
	return fmt.Sprintf("http://%s/api/v1/namespaces/%s/services/http:%s:/proxy/", proxy, ns, svc)
}

// checkURL checks if a URL returns 200 HTTP OK
func checkURL(url string) error {
	resp, err := http.Get(url)
	glog.Infof("%s response: %v %+v", url, err, resp)
	if err != nil {
		return errors.Wrap(err, "checkURL")
	}
	if resp.StatusCode != http.StatusOK {
		return &util.RetriableError{
			Err: fmt.Errorf("unexpected response code: %d", resp.StatusCode),
		}
	}
	return nil
}

func init() {
	dashboardCmd.Flags().BoolVar(&dashboardURLMode, "url", false, "Display dashboard URL instead of opening a browser")
}
