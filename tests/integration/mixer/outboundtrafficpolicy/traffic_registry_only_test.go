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

package outboundtrafficpolicy

import (
	"testing"
)

func TestOutboundTrafficPolicy_RegistryOnly(t *testing.T) {
	cases := []*TestCase{
		{
			Name:     "HTTP Traffic",
			PortName: "http",
			Expected: Expected{
				Metric:          "istio_requests_total",
				PromQueryFormat: `sum(istio_requests_total{destination_service_name="BlackHoleCluster",response_code="502"})`,
				ResponseCode:    []string{"502"},
			},
		},
		{
			Name:     "HTTPS Traffic",
			PortName: "https",
			Expected: Expected{
				Metric:          "istio_tcp_connections_closed_total",
				PromQueryFormat: `sum(istio_tcp_connections_closed_total{destination_service="BlackHoleCluster",destination_service_name="BlackHoleCluster"})`,
				ResponseCode:    []string{},
			},
		},
		{
			Name:     "HTTPS Traffic Conflict",
			PortName: "https-conflict",
			Expected: Expected{
				Metric:          "istio_tcp_connections_closed_total",
				PromQueryFormat: `sum(istio_tcp_connections_closed_total{destination_service="BlackHoleCluster",destination_service_name="BlackHoleCluster"})`,
				ResponseCode:    []string{},
			},
		},
		{
			Name:     "HTTP Traffic Egress",
			PortName: "http",
			Host:     "some-external-site.com",
			Gateway:  true,
			Expected: Expected{
				Metric:          "istio_requests_total",
				PromQueryFormat: `sum(istio_requests_total{destination_service_name="istio-egressgateway",response_code="200"})`,
				ResponseCode:    []string{"200"},
			},
		},
		// TODO add HTTPS through gateway
		{
			Name:     "TCP",
			PortName: "tcp",
			Expected: Expected{
				// TODO(https://github.com/istio/istio/issues/22735) add metrics
				Metric:          "",
				PromQueryFormat: "",
				ResponseCode:    []string{},
			},
		},
		{
			Name:     "TCP Conflict",
			PortName: "tcp-conflict",
			Expected: Expected{
				// TODO(https://github.com/istio/istio/issues/22735) add metrics
				Metric:          "",
				PromQueryFormat: "",
				ResponseCode:    []string{},
			},
		},
	}

	RunExternalRequest(cases, prom, RegistryOnly, t)

}
