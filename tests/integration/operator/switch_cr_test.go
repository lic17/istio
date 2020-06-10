// Copyright Istio Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package operator

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/golang/protobuf/jsonpb"
	"k8s.io/apimachinery/pkg/runtime/schema"

	api "istio.io/api/operator/v1alpha1"
	"istio.io/istio/operator/pkg/object"
	"istio.io/istio/operator/pkg/util"
	"istio.io/istio/pkg/config/protocol"
	"istio.io/istio/pkg/test/env"
	"istio.io/istio/pkg/test/framework"
	"istio.io/istio/pkg/test/framework/components/echo"
	"istio.io/istio/pkg/test/framework/components/echo/echoboot"
	"istio.io/istio/pkg/test/framework/components/environment/kube"
	"istio.io/istio/pkg/test/framework/components/istioctl"
	"istio.io/istio/pkg/test/framework/components/namespace"
	"istio.io/istio/pkg/test/framework/image"
	"istio.io/istio/pkg/test/framework/resource"
	"istio.io/istio/pkg/test/framework/resource/environment"
	"istio.io/istio/pkg/test/scopes"
	"istio.io/istio/pkg/test/util/retry"
	"istio.io/pkg/log"
)

const (
	IstioNamespace    = "istio-system"
	OperatorNamespace = "istio-operator"
	retryDelay        = time.Second
	retryTimeOut      = 20 * time.Minute
)

var (
	// ManifestPath is path of local manifests which istioctl operator init refers to.
	ManifestPath = filepath.Join(env.IstioSrc, "manifests")
	// ManifestPathContainer is path of manifests in the operator container for controller to work with.
	ManifestPathContainer = "/var/lib/istio/manifests"
	iopCRFile             = ""
)

func TestController(t *testing.T) {
	framework.
		NewTest(t).
		RequiresEnvironment(environment.Kube).
		Run(func(ctx framework.TestContext) {
			istioCtl := istioctl.NewOrFail(ctx, ctx, istioctl.Config{})
			workDir, err := ctx.CreateTmpDirectory("operator-controller-test")
			if err != nil {
				t.Fatal("failed to create test directory")
			}
			cs := ctx.Environment().(*kube.Environment).KubeClusters[0]
			s, err := image.SettingsFromCommandLine()
			if err != nil {
				t.Fatal(err)
			}
			// clean up hanging installed-state CR from previous tests
			gvr := schema.GroupVersionResource{
				Group:    "install.istio.io",
				Version:  "v1alpha1",
				Resource: "istiooperators",
			}
			if err := cs.DeleteUnstructured(gvr, IstioNamespace, "installed-state"); err != nil {
				t.Logf(err.Error())
			}
			initCmd := []string{
				"operator", "init",
				"--hub=" + s.Hub,
				"--tag=" + s.Tag,
				"--charts=" + ManifestPath,
			}
			// install istio with default config for the first time by running operator init command
			istioCtl.InvokeOrFail(t, initCmd)

			if err := cs.CreateNamespace(IstioNamespace, ""); err != nil {
				_, err := cs.GetNamespace(IstioNamespace)
				if err == nil {
					log.Info("istio namespace already exist")
				} else {
					t.Errorf("failed to create istio namespace: %v", err)
				}
			}
			iopCRFile = filepath.Join(workDir, "iop_cr.yaml")
			// later just run `kubectl apply -f newcr.yaml` to apply new installation cr files and verify.
			installWithCRFile(t, ctx, cs, s, istioCtl, "default")
			installWithCRFile(t, ctx, cs, s, istioCtl, "demo")
			// cleanup created resources
			t.Cleanup(func() {
				scopes.CI.Infof("cleaning up resources")
				if err := cs.Delete(IstioNamespace, iopCRFile); err != nil {
					t.Errorf("faild to delete test IstioOperator CR: %v", err)
				}
				if err := cs.DeleteNamespace(OperatorNamespace); err != nil {
					t.Errorf("failed to delete operator namespace: %v", err)
				}
			})
		})
}

// checkInstallStatus check the status of IstioOperator CR from the cluster
func checkInstallStatus(cs kube.Cluster) error {
	scopes.CI.Infof("checking IstioOperator CR status")
	gvr := schema.GroupVersionResource{
		Group:    "install.istio.io",
		Version:  "v1alpha1",
		Resource: "istiooperators",
	}

	var unhealthyCN []string
	retryFunc := func() error {
		us, err := cs.GetUnstructured(gvr, IstioNamespace, "test-istiocontrolplane")
		if err != nil {
			return fmt.Errorf("failed to get istioOperator resource: %v", err)
		}
		usIOPStatus := us.UnstructuredContent()["status"]
		if usIOPStatus == nil {
			if _, err := cs.GetService(OperatorNamespace, "istio-operator"); err != nil {
				return fmt.Errorf("istio operator svc is not ready: %v", err)
			}
			if _, err := cs.CheckPodsAreReady(cs.NewPodFetch(OperatorNamespace)); err != nil {
				return fmt.Errorf("istio operator pod is not ready: %v", err)
			}

			return fmt.Errorf("status not found from the istioOperator resource")
		}
		usIOPStatus = usIOPStatus.(map[string]interface{})
		iopStatusString, err := json.Marshal(usIOPStatus)
		if err != nil {
			return fmt.Errorf("failed to marshal istioOperator status: %v", err)
		}
		status := &api.InstallStatus{}
		jspb := jsonpb.Unmarshaler{AllowUnknownFields: true}
		if err := jspb.Unmarshal(bytes.NewReader(iopStatusString), status); err != nil {
			return fmt.Errorf("failed to unmarshal istioOperator status: %v", err)
		}
		errs := util.Errors{}
		unhealthyCN = []string{}
		if status.Status != api.InstallStatus_HEALTHY {
			errs = util.AppendErr(errs, fmt.Errorf("got IstioOperator status: %v", status.Status))
		}

		for cn, cnstatus := range status.ComponentStatus {
			if cnstatus.Status != api.InstallStatus_HEALTHY {
				unhealthyCN = append(unhealthyCN, cn)
				errs = util.AppendErr(errs, fmt.Errorf("got component: %s status: %v", cn, cnstatus.Status))
			}
		}
		return errs.ToError()
	}
	err := retry.UntilSuccess(retryFunc, retry.Timeout(retryTimeOut), retry.Delay(retryDelay))
	if err != nil {
		return fmt.Errorf("istioOperator status is not healthy: %v", err)
	}
	return nil
}

func installWithCRFile(t *testing.T, ctx resource.Context, cs kube.Cluster, s *image.Settings,
	istioCtl istioctl.Instance, profileName string) {
	scopes.CI.Infof(fmt.Sprintf("=== install istio with profile: %s===\n", profileName))
	metadataYAML := `
apiVersion: install.istio.io/v1alpha1
kind: IstioOperator
metadata:
  name: test-istiocontrolplane
  namespace: istio-system
spec:
  profile: %s
  installPackagePath: %s
  hub: %s
  tag: %s
  values:
    global:
      imagePullPolicy: %s
`
	overlayYAML := fmt.Sprintf(metadataYAML, profileName, ManifestPathContainer, s.Hub, s.Tag, s.PullPolicy)
	if err := ioutil.WriteFile(iopCRFile, []byte(overlayYAML), os.ModePerm); err != nil {
		t.Fatalf("failed to write iop cr file: %v", err)
	}

	if err := cs.Apply(IstioNamespace, iopCRFile); err != nil {
		t.Fatalf("failed to apply IstioOperator CR file: %s, %v", iopCRFile, err)
	}

	verifyInstallation(t, ctx, istioCtl, profileName, cs)
}

// verifyInstallation verify IOP CR status and compare in-cluster resources with generated ones.
func verifyInstallation(t *testing.T, ctx resource.Context,
	istioCtl istioctl.Instance, profileName string, cs kube.Cluster) {
	scopes.CI.Infof("=== verifying istio installation === ")
	if err := checkInstallStatus(cs); err != nil {
		t.Fatalf("IstioOperator status not healthy: %v", err)
	}

	if _, err := cs.CheckPodsAreReady(cs.NewSinglePodFetch(IstioNamespace, "app=istiod")); err != nil {
		t.Fatalf("istiod pod is not ready: %v", err)
	}

	if err := compareInClusterAndGeneratedResources(t, istioCtl, profileName, cs); err != nil {
		t.Fatalf("in cluster resources does not match with the generated ones: %v", err)
	}
	sanityCheck(t, ctx)
	scopes.CI.Infof("=== succeeded ===")
}

func sanityCheck(t *testing.T, ctx resource.Context) {
	scopes.CI.Infof("running sanity test")
	var client, server echo.Instance
	test := namespace.NewOrFail(t, ctx, namespace.Config{
		Prefix: "default",
		Inject: true,
	})
	echoboot.NewBuilderOrFail(t, ctx).
		With(&client, echo.Config{
			Service:   "client",
			Namespace: test,
			Ports:     []echo.Port{},
		}).
		With(&server, echo.Config{
			Service:   "server",
			Namespace: test,
			Ports: []echo.Port{
				{
					Name:         "http",
					Protocol:     protocol.HTTP,
					InstancePort: 8090,
				}},
		}).
		BuildOrFail(t)
	retry.UntilSuccessOrFail(t, func() error {
		resp, err := client.Call(echo.CallOptions{
			Target:   server,
			PortName: "http",
		})
		if err != nil {
			return err
		}
		return resp.CheckOK()
	}, retry.Delay(time.Millisecond*100), retry.Timeout(retryTimeOut))
}

func compareInClusterAndGeneratedResources(t *testing.T, istioCtl istioctl.Instance, profileName string,
	cs kube.Cluster) error {
	// get manifests by running `manifest generate`
	generateCmd := []string{
		"manifest", "generate",
		"--charts", ManifestPath,
	}
	if profileName != "" {
		generateCmd = append(generateCmd, "--set", fmt.Sprintf("profile=%s", profileName))
	}
	genManifests, _ := istioCtl.InvokeOrFail(t, generateCmd)
	genK8SObjects, err := object.ParseK8sObjectsFromYAMLManifest(genManifests)
	if err != nil {
		return fmt.Errorf("failed to parse generated manifest: %v", err)
	}
	efgvr := schema.GroupVersionResource{
		Group:    "networking.istio.io",
		Version:  "v1alpha3",
		Resource: "envoyfilters",
	}

	for _, genK8SObject := range genK8SObjects {
		kind := genK8SObject.Kind
		ns := genK8SObject.Namespace
		name := genK8SObject.Name
		log.Infof("checking kind: %s, namespace: %s, name: %s", kind, ns, name)
		retry.UntilSuccessOrFail(t, func() error {
			switch kind {
			case "Service":
				if _, err := cs.GetService(ns, name); err != nil {
					return fmt.Errorf("failed to get expected service: %s from cluster", name)
				}
			case "ServiceAccount":
				if _, err := cs.GetServiceAccount(ns, name); err != nil {
					return fmt.Errorf("failed to get expected serviceAccount: %s from cluster", name)
				}
			case "Deployment":
				if _, err := cs.GetDeployment(IstioNamespace, name); err != nil {
					return fmt.Errorf("failed to get expected deployment: %s from cluster", name)
				}
			case "ConfigMap":
				if _, err := cs.GetConfigMap(name, ns); err != nil {
					return fmt.Errorf("failed to get expected configMap: %s from cluster", name)
				}
			case "ValidatingWebhookConfiguration":
				if exist := cs.ValidatingWebhookConfigurationExists(name); !exist {
					return fmt.Errorf("failed to get expected ValidatingWebhookConfiguration: %s from cluster", name)
				}
			case "MutatingWebhookConfiguration":
				if exist := cs.MutatingWebhookConfigurationExists(name); !exist {
					return fmt.Errorf("failed to get expected MutatingWebhookConfiguration: %s from cluster", name)
				}
			case "CustomResourceDefinition":
				if _, err := cs.GetCustomResourceDefinition(name); err != nil {
					return fmt.Errorf("failed to get expected CustomResourceDefinition: %s from cluster", name)
				}
			case "EnvoyFilter":
				if _, err := cs.GetUnstructured(efgvr, ns, name); err != nil {
					return fmt.Errorf("failed to get expected Envoyfilter: %s from cluster", name)
				}
			case "PodDisruptionBudget":
				if _, err := cs.GetPodDisruptionBudget(ns, name); err != nil {
					return fmt.Errorf("failed to get expected PodDisruptionBudget: %s from cluster", name)
				}
			case "HorizontalPodAutoscaler":
				if _, err := cs.GetHorizontalPodAutoscaler(ns, name); err != nil {
					return fmt.Errorf("failed to get expected HorizontalPodAutoscaler: %s from cluster", name)
				}
			}
			return nil
		}, retry.Timeout(time.Second*300), retry.Delay(time.Millisecond*100))
	}
	return nil
}
