//go:build integ
// +build integ

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

package uproxy

import (
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"istio.io/api/networking/v1alpha3"
	"istio.io/istio/pkg/http/headers"
	echot "istio.io/istio/pkg/test/echo"
	"istio.io/istio/pkg/test/echo/common/scheme"
	"istio.io/istio/pkg/test/framework"
	"istio.io/istio/pkg/test/framework/components/echo"
	"istio.io/istio/pkg/test/framework/components/echo/check"
	"istio.io/istio/pkg/test/framework/components/echo/common"
	"istio.io/istio/pkg/test/framework/components/echo/common/ports"
	"istio.io/istio/pkg/test/framework/components/echo/config"
	"istio.io/istio/pkg/test/framework/components/echo/config/param"
	"istio.io/istio/pkg/test/framework/components/echo/echotest"
	"istio.io/istio/pkg/test/framework/components/echo/match"
	"istio.io/istio/pkg/test/framework/components/istio"
	"istio.io/istio/pkg/test/framework/components/istio/ingress"
	"istio.io/istio/pkg/test/framework/components/prometheus"
	"istio.io/istio/pkg/test/framework/resource/config/apply"
	"istio.io/istio/pkg/test/util/retry"
	"istio.io/istio/pkg/util/sets"
	"istio.io/istio/tests/integration/security/util/reachability"
	util "istio.io/istio/tests/integration/telemetry"
)

func IsL7() echo.Checker {
	return check.Each(func(r echot.Response) error {
		// TODO: response headers?
		_, f := r.RequestHeaders[http.CanonicalHeaderKey("x-b3-traceid")]
		if !f {
			return fmt.Errorf("x-b3-traceid not set, is L7 processing enabled?")
		}
		return nil
	})
}

func IsL4() echo.Checker {
	return check.Each(func(r echot.Response) error {
		// TODO: response headers?
		_, f := r.RequestHeaders[http.CanonicalHeaderKey("x-b3-traceid")]
		if f {
			return fmt.Errorf("x-b3-traceid set, is L7 processing enabled unexpectedly?")
		}
		return nil
	})
}

var (
	httpValidator = check.And(check.OK(), IsL7())
	tcpValidator  = check.And(check.OK(), IsL4())
	callOptions   = []echo.CallOptions{
		{
			Port:   echo.Port{Name: "http"},
			Scheme: scheme.HTTP,
			Count:  10, // TODO use more
		},
		//{
		//	Port: echo.Port{Name: "http"},
		//	Scheme:   scheme.WebSocket,
		//	Count:    4,
		//	Timeout:  time.Second * 2,
		//},
		{
			Port:   echo.Port{Name: "tcp"},
			Scheme: scheme.TCP,
			Count:  1,
		},
		//{
		//	Port: echo.Port{Name: "grpc"},
		//	Scheme:   scheme.GRPC,
		//	Count:    4,
		//	Timeout:  time.Second * 2,
		//},
		//{
		//	Port: echo.Port{Name: "https"},
		//	Scheme:   scheme.HTTPS,
		//	Count:    4,
		//	Timeout:  time.Second * 2,
		//},
	}
)

func supportsL7(opt echo.CallOptions, instances ...echo.Instance) bool {
	hasRemote := false
	hasSidecar := false
	for _, i := range instances {
		hasRemote = hasRemote || i.Config().IsRemote()
		hasSidecar = hasSidecar || i.Config().HasSidecar()
	}
	isL7Scheme := opt.Scheme == scheme.HTTP || opt.Scheme == scheme.GRPC || opt.Scheme == scheme.WebSocket
	return (hasRemote || hasSidecar) && isL7Scheme
}

func TestServices(t *testing.T) {
	runTest(t, func(t framework.TestContext, src echo.Instance, dst echo.Instance, opt echo.CallOptions) {
		if supportsL7(opt, src, dst) {
			opt.Check = httpValidator
		} else {
			opt.Check = tcpValidator
		}

		if src.Config().IsUncaptured() && dst.Config().IsRemote() {
			// For this case, it is broken if the src and dst are on the same node.
			// Because client request is not captured to perform the hairpin
			// TODO: fix this and remove this skip
			opt.Check = check.OK()
		}
		// TODO test from all source workloads as well
		src.CallOrFail(t, opt)
	})
}

func TestPodIP(t *testing.T) {
	t.Skip("https://github.com/solo-io/istio-sidecarless/issues/106")
	framework.NewTest(t).Run(func(t framework.TestContext) {
		for _, src := range apps.All {
			for _, srcWl := range src.WorkloadsOrFail(t) {
				srcWl := srcWl
				t.NewSubTestf("from %v %v", src.Config().Service, srcWl.Address()).Run(func(t framework.TestContext) {
					for _, dst := range apps.All {
						for _, dstWl := range dst.WorkloadsOrFail(t) {
							t.NewSubTestf("to %v %v", dst.Config().Service, dstWl.Address()).Run(func(t framework.TestContext) {
								src, dst, srcWl, dstWl := src, dst, srcWl, dstWl
								if src.Config().IsRemote() || dst.Config().IsRemote() {
									t.Skip("not supported yet")
								}
								for _, opt := range callOptions {
									opt := opt.DeepCopy()
									selfSend := dstWl.Address() == srcWl.Address()
									if supportsL7(opt, src, dst) {
										opt.Check = httpValidator
									} else {
										opt.Check = tcpValidator
									}
									if selfSend {
										// Calls to ourself (by pod IP) are not captured
										opt.Check = tcpValidator
									} else if src.Config().IsUncaptured() && dst.Config().IsRemote() {
										// For this case, it is broken if the src and dst are on the same node.
										// Because client request is not captured to perform the hairpin
										// TODO: fix this and remove this skip
										opt.Check = check.OK()
									}
									opt.Address = dstWl.Address()
									opt.Check = check.And(opt.Check, check.Hostname(dstWl.PodName()))

									opt.Port = echo.Port{ServicePort: ports.All().MustForName(opt.Port.Name).WorkloadPort}
									opt.ToWorkload = dst.WithWorkloads(dstWl)
									t.NewSubTestf("%v", opt.Scheme).RunParallel(func(t framework.TestContext) {
										src.WithWorkloads(srcWl).CallOrFail(t, opt)
									})
								}
							})
						}
					}
				})
			}
		}
	})
}

func TestServerSideLB(t *testing.T) {
	// TODO: test that naked client reusing connections will load balance
	runTest(t, func(t framework.TestContext, src echo.Instance, dst echo.Instance, opt echo.CallOptions) {
		// Need HTTP
		if opt.Scheme != scheme.HTTP {
			return
		}
		if src.Config().IsUncaptured() {
			// For this case, it is broken if the src and dst are on the same node.
			// TODO: fix this and remove this skip
			t.Skip("broken")
		}
		var singleHost echo.Checker = func(result echo.CallResult, _ error) error {
			hostnames := make([]string, len(result.Responses))
			for i, r := range result.Responses {
				hostnames[i] = r.Hostname
			}
			unique := sets.New(hostnames...).SortedList()
			if len(unique) != 1 {
				return fmt.Errorf("excepted only one destination, got: %v", unique)
			}
			return nil
		}
		var multipleHost echo.Checker = func(result echo.CallResult, _ error) error {
			hostnames := make([]string, len(result.Responses))
			for i, r := range result.Responses {
				hostnames[i] = r.Hostname
			}
			unique := sets.New(hostnames...).SortedList()
			want := dst.WorkloadsOrFail(t).Len()
			if len(unique) != want {
				return fmt.Errorf("excepted all destinations (%v), got: %v", want, unique)
			}
			return nil
		}

		shouldBalance := dst.Config().IsRemote() || src.Config().IsRemote()
		// Istio client will not reuse connections for HTTP/1.1
		opt.HTTP.HTTP2 = true
		// Make sure we make multiple calls
		opt.Count = 10
		c := singleHost
		if shouldBalance {
			c = multipleHost
		}
		opt.Check = check.And(check.OK(), c)
		opt.NewConnectionPerRequest = false
		src.CallOrFail(t, opt)
	})
}

func TestServerRouting(t *testing.T) {
	runTest(t, func(t framework.TestContext, src echo.Instance, dst echo.Instance, opt echo.CallOptions) {
		// Need at least one remote and HTTP
		if opt.Scheme != scheme.HTTP {
			return
		}
		if !dst.Config().IsRemote() && !src.Config().IsRemote() {
			return
		}
		if src.Config().IsUncaptured() {
			// For this case, it is broken if the src and dst are on the same node.
			// TODO: fix this and remove this skip
			t.Skip("https://github.com/solo-io/istio-sidecarless/issues/103")
		}
		t.ConfigIstio().Eval(apps.Namespace.Name(), map[string]string{
			"Destination": dst.Config().Service,
		}, `apiVersion: networking.istio.io/v1alpha3
kind: VirtualService
metadata:
  name: route
spec:
  hosts:
  - "{{.Destination}}"
  http:
  - headers:
      request:
        add:
          istio-custom-header: user-defined-value
    route:
    - destination:
        host: "{{.Destination}}"
`).ApplyOrFail(t)
		opt.Check = check.And(
			check.OK(),
			check.RequestHeader("Istio-Custom-Header", "user-defined-value"))
		src.CallOrFail(t, opt)
	})
}

func TestTrafficSplit(t *testing.T) {
	runTest(t, func(t framework.TestContext, src echo.Instance, dst echo.Instance, opt echo.CallOptions) {
		// Need at least one remote and HTTP
		if opt.Scheme != scheme.HTTP {
			return
		}
		if !dst.Config().IsRemote() && !src.Config().IsRemote() {
			return
		}
		if src.Config().IsUncaptured() {
			// For this case, it is broken if the src and dst are on the same node.
			// TODO: fix this and remove this skip
			t.Skip("https://github.com/solo-io/istio-sidecarless/issues/103")
		}
		t.ConfigIstio().Eval(apps.Namespace.Name(), map[string]string{
			"Destination": dst.Config().Service,
		}, `apiVersion: networking.istio.io/v1alpha3
kind: VirtualService
metadata:
  name: route
spec:
  hosts:
  - "{{.Destination}}"
  http:
  - match:
    - headers:
        user:
          exact: istio-custom-user
    route:
    - destination:
        host: "{{.Destination}}"
        subset: v2
  - route:
    - destination:
        host: "{{.Destination}}"
        subset: v1
`).ApplyOrFail(t)
		t.ConfigIstio().Eval(apps.Namespace.Name(), map[string]string{
			"Destination": dst.Config().Service,
		}, `apiVersion: networking.istio.io/v1alpha3
kind: DestinationRule
metadata:
  name: dr
spec:
  host: "{{.Destination}}"
  subsets:
  - name: v1
    labels:
      version: v1
  - name: v2
    labels:
      version: v2
`).ApplyOrFail(t)
		t.NewSubTest("v1").Run(func(t framework.TestContext) {
			opt = opt.DeepCopy()
			opt.Count = 5
			opt.Timeout = time.Second * 10
			opt.Check = check.And(
				check.OK(),
				func(result echo.CallResult, _ error) error {
					for _, r := range result.Responses {
						if r.Version != "v1" {
							return fmt.Errorf("expected service version %q, got %q", "v1", r.Version)
						}
					}
					return nil
				})
			src.CallOrFail(t, opt)
		})

		t.NewSubTest("v2").Run(func(t framework.TestContext) {
			opt = opt.DeepCopy()
			opt.Count = 5
			opt.Timeout = time.Second * 10
			if opt.HTTP.Headers == nil {
				opt.HTTP.Headers = map[string][]string{}
			}
			opt.HTTP.Headers.Set("user", "istio-custom-user")
			opt.Check = check.And(
				check.OK(),
				func(result echo.CallResult, _ error) error {
					for _, r := range result.Responses {
						if r.Version != "v2" {
							return fmt.Errorf("expected service version %q, got %q", "v2", r.Version)
						}
					}
					return nil
				})
			src.CallOrFail(t, opt)
		})
	})
}

func TestAuthorizationL4(t *testing.T) {
	framework.NewTest(t).Features("traffic.ambient").Run(func(t framework.TestContext) {
		// Workaround https://github.com/solo-io/istio-sidecarless/issues/287
		t.ConfigIstio().YAML(apps.Namespace.Name(), `apiVersion: networking.istio.io/v1alpha3
kind: DestinationRule
metadata:
  name: single-request
spec:
  host: '*.svc.cluster.local'
  trafficPolicy:
    connectionPool:
      http:
        maxRequestsPerConnection: 1`).ApplyOrFail(t)
		runTestContext(t, func(t framework.TestContext, src echo.Instance, dst echo.Instance, opt echo.CallOptions) {
			if opt.Scheme != scheme.TCP {
				return
			}
			// Ensure we don't get stuck on old connections with old RBAC rules. This causes 45s test times
			// due to draining.
			opt.NewConnectionPerRequest = true
			if src.Config().IsUncaptured() {
				// For this case, it is broken if the src and dst are on the same node.
				// TODO: fix this and remove this skip
				t.Skip("https://github.com/solo-io/istio-sidecarless/issues/103")
			}

			overrideCheck := func(opt *echo.CallOptions) {
				switch {
				case src.Config().IsUncaptured() && dst.Config().IsRemote():
					// For this case, it is broken if the src and dst are on the same node.
					// Because client request is not captured to perform the hairpin
					// TODO: fix this and remove this skip
					opt.Check = check.OK()
				case dst.Config().IsUncaptured() && !dst.Config().HasSidecar():
					// No destination means no RBAC to apply. Make sure we do not accidentally reject
					opt.Check = check.OK()
				}
			}
			t.NewSubTest("allow").Run(func(t framework.TestContext) {
				t.ConfigIstio().Eval(apps.Namespace.Name(), map[string]string{
					"Destination": dst.Config().Service,
					"Source":      src.Config().Service,
					"Namespace":   apps.Namespace.Name(),
				}, `apiVersion: security.istio.io/v1beta1
kind: AuthorizationPolicy
metadata:
  name: policy
spec:
  selector:
    matchLabels:
      app: "{{ .Destination }}"
  rules:
  - from:
    - source:
        principals: ["cluster.local/ns/{{.Namespace}}/sa/{{.Source}}"]
`).ApplyOrFail(t)
				opt = opt.DeepCopy()
				overrideCheck(&opt)
				src.CallOrFail(t, opt)
			})
			t.NewSubTest("not allow").Run(func(t framework.TestContext) {
				t.ConfigIstio().Eval(apps.Namespace.Name(), map[string]string{
					"Destination": dst.Config().Service,
					"Source":      src.Config().Service,
					"Namespace":   apps.Namespace.Name(),
				}, `apiVersion: security.istio.io/v1beta1
kind: AuthorizationPolicy
metadata:
  name: policy
spec:
  selector:
    matchLabels:
      app: "{{ .Destination }}"
  rules:
  - from:
    - source:
        principals: ["cluster.local/ns/something/sa/else"]
`).ApplyOrFail(t)
				opt = opt.DeepCopy()
				opt.Check = CheckDeny
				overrideCheck(&opt)
				src.CallOrFail(t, opt)
			})
		})
	})
}

func TestAuthorizationL7(t *testing.T) {
	framework.NewTest(t).Features("traffic.ambient").Run(func(t framework.TestContext) {
		// Workaround https://github.com/solo-io/istio-sidecarless/issues/287
		t.ConfigIstio().YAML(apps.Namespace.Name(), `apiVersion: networking.istio.io/v1alpha3
kind: DestinationRule
metadata:
  name: single-request
spec:
  host: '*.svc.cluster.local'
  trafficPolicy:
    connectionPool:
      http:
        maxRequestsPerConnection: 1`).ApplyOrFail(t)
		runTestContext(t, func(t framework.TestContext, src echo.Instance, dst echo.Instance, opt echo.CallOptions) {
			if opt.Scheme != scheme.HTTP {
				return
			}
			// Ensure we don't get stuck on old connections with old RBAC rules. This causes 45s test times
			// due to draining.
			opt.NewConnectionPerRequest = true
			if src.Config().IsUncaptured() {
				// For this case, it is broken if the src and dst are on the same node.
				// TODO: fix this and remove this skip
				t.Skip("https://github.com/solo-io/istio-sidecarless/issues/103")
			}
			t.ConfigIstio().Eval(apps.Namespace.Name(), map[string]string{
				"Destination": dst.Config().Service,
				"Source":      src.Config().Service,
				"Namespace":   apps.Namespace.Name(),
			}, `apiVersion: security.istio.io/v1beta1
kind: AuthorizationPolicy
metadata:
  name: policy
spec:
  selector:
    matchLabels:
      app: "{{ .Destination }}"
  rules:
  - to:
    - operation:
        paths: ["/allowed"]
        methods: ["GET"]
  - from:
    - source:
        principals: ["cluster.local/ns/{{.Namespace}}/sa/{{.Source}}"]
    to:
    - operation:
        paths: ["/allowed-identity"]
        methods: ["GET"]
  - from:
    - source:
        principals: ["cluster.local/ns/{{.Namespace}}/sa/someone-else"]
    to:
    - operation:
        paths: ["/denied-identity"]
        methods: ["GET"]
`).ApplyOrFail(t)
			overrideCheck := func(opt *echo.CallOptions) {
				switch {
				case src.Config().IsUncaptured() && dst.Config().IsRemote():
					// For this case, it is broken if the src and dst are on the same node.
					// Because client request is not captured to perform the hairpin
					// TODO: fix this and remove this skip
					opt.Check = check.OK()
				case dst.Config().IsUncaptured() && !dst.Config().HasSidecar():
					// No destination means no RBAC to apply. Make sure we do not accidentally reject
					opt.Check = check.OK()
				case !dst.Config().IsRemote() && !dst.Config().HasSidecar():
					// Only remote can handle L7 policies
					opt.Check = CheckDeny
				}
			}
			t.NewSubTest("simple deny").Run(func(t framework.TestContext) {
				opt = opt.DeepCopy()
				opt.HTTP.Path = "/deny"
				opt.Check = CheckDeny
				overrideCheck(&opt)
				src.CallOrFail(t, opt)
			})
			t.NewSubTest("simple allow").Run(func(t framework.TestContext) {
				opt = opt.DeepCopy()
				opt.HTTP.Path = "/allowed"
				opt.Check = check.OK()
				overrideCheck(&opt)
				src.CallOrFail(t, opt)
			})
			t.NewSubTest("identity deny").Run(func(t framework.TestContext) {
				opt = opt.DeepCopy()
				opt.HTTP.Path = "/denied-identity"
				opt.Check = CheckDeny
				overrideCheck(&opt)
				src.CallOrFail(t, opt)
			})
			t.NewSubTest("identity allow").Run(func(t framework.TestContext) {
				opt = opt.DeepCopy()
				opt.HTTP.Path = "/allowed-identity"
				opt.Check = check.OK()
				overrideCheck(&opt)
				src.CallOrFail(t, opt)
			})
		})
	})
}

func TestMTLS(t *testing.T) {
	framework.NewTest(t).
		Features("security.reachability").
		Run(func(t framework.TestContext) {
			systemNM := istio.ClaimSystemNamespaceOrFail(t, t)
			// mtlsOnExpect defines our expectations for when mTLS is expected when its enabled
			mtlsOnExpect := func(from echo.Instance, opts echo.CallOptions) bool {
				if from.Config().IsNaked() || opts.To.Config().IsNaked() {
					// If one of the two endpoints is naked, we don't send mTLS
					return false
				}
				if opts.To.Config().IsHeadless() && opts.To.Instances().Contains(from) {
					// pod calling its own pod IP will not be intercepted
					return false
				}
				return true
			}
			Always := func(echo.Instance, echo.CallOptions) bool {
				return true
			}
			Never := func(echo.Instance, echo.CallOptions) bool {
				return false
			}
			SameNetwork := func(from echo.Instance, to echo.Target) echo.Instances {
				return match.Network(from.Config().Cluster.NetworkName()).GetMatches(to.Instances())
			}
			_ = Never
			_ = SameNetwork
			testCases := []reachability.TestCase{
				{
					ConfigFile: "beta-mtls-on.yaml",
					Namespace:  systemNM,
					Include:    Always,
					ExpectSuccess: func(from echo.Instance, opts echo.CallOptions) bool {
						if from.Config().HasProxyCapabilities() != opts.To.Config().HasProxyCapabilities() {
							if from.Config().HasProxyCapabilities() && !from.Config().IsRemote() {
								if from.Config().HasSidecar() && !opts.To.Config().HasProxyCapabilities() {
									// Sidecar respects it ISTIO_MUTUAL, will only send mTLS
									return false
								}
								return true
							}
							if !from.Config().HasProxyCapabilities() && opts.To.Config().IsRemote() {
								// TODO: support hairpin
								return true
							}
							if !from.Config().HasProxyCapabilities() && !opts.To.Config().HasSidecar() {
								// TODO: https://github.com/solo-io/istio-sidecarless/issues/243
								return true
							}
							return false
						}
						if !from.Config().HasProxyCapabilities() && opts.To.Config().HasSidecar() {
							return false
						}
						return true
					},
					ExpectMTLS: mtlsOnExpect,
				},
				{
					ConfigFile: "beta-mtls-permissive.yaml",
					Namespace:  systemNM,
					Include: func(_ echo.Instance, opts echo.CallOptions) bool {
						// Exclude calls to naked since we are applying ISTIO_MUTUAL
						return !opts.To.Config().IsNaked()
					},
					ExpectSuccess: func(from echo.Instance, opts echo.CallOptions) bool {
						if (from.Config().IsRemote() || from.Config().HasSidecar()) && !opts.To.Config().HasProxyCapabilities() {
							return false
						}
						return true
					},
					ExpectMTLS: mtlsOnExpect,
				},
				{
					ConfigFile:    "beta-mtls-off.yaml",
					Namespace:     systemNM,
					Include:       Always,
					ExpectSuccess: Always,
					ExpectMTLS:    Never,
					// Without TLS we can't perform SNI routing required for multi-network
					ExpectDestinations: SameNetwork,
				},
				{
					ConfigFile:    "plaintext-to-permissive.yaml",
					Namespace:     systemNM,
					Include:       Always,
					ExpectSuccess: Always,
					ExpectMTLS:    Never,
					// Since we are only sending plaintext and Without TLS
					// we can't perform SNI routing required for multi-network
					ExpectDestinations: SameNetwork,
				},
				{
					ConfigFile: "beta-mtls-automtls.yaml",
					Namespace:  apps.Namespace,
					Include:    Always,
					ExpectSuccess: func(from echo.Instance, opts echo.CallOptions) bool {
						if !from.Config().HasProxyCapabilities() && !opts.To.Config().HasSidecar() {
							// TODO: https://github.com/solo-io/istio-sidecarless/issues/243
							return true
						}
						// autoMtls doesn't work for client that doesn't have proxy, unless target doesn't
						// have proxy neither.
						if !from.Config().HasProxyCapabilities() {
							return !opts.To.Config().HasProxyCapabilities()
						}
						return true
					},
					ExpectMTLS: mtlsOnExpect,
				},
				{
					ConfigFile: "no-peer-authn.yaml",
					Namespace:  systemNM,
					Include: func(_ echo.Instance, opts echo.CallOptions) bool {
						// Exclude calls to naked since we are applying ISTIO_MUTUAL
						return !opts.To.Config().IsNaked()
					},
					ExpectSuccess: func(from echo.Instance, opts echo.CallOptions) bool {
						if from.Config().HasSidecar() && !opts.To.Config().HasProxyCapabilities() {
							// Sidecar respects it
							return false
						}
						if from.Config().IsRemote() && !opts.To.Config().HasProxyCapabilities() {
							// Remote respects it
							return false
						}
						return true
					},
					ExpectMTLS: mtlsOnExpect,
				},
				{
					ConfigFile: "global-plaintext.yaml",
					Namespace:  systemNM,
					ExpectDestinations: func(from echo.Instance, to echo.Target) echo.Instances {
						// Without TLS we can't perform SNI routing required for multi-network
						return match.Network(from.Config().Cluster.NetworkName()).GetMatches(to.Instances())
					},
					ExpectSuccess: Always,
					ExpectMTLS:    Never,
				},
				{
					ConfigFile: "automtls-passthrough.yaml",
					Namespace:  systemNM,
					Include: func(_ echo.Instance, opts echo.CallOptions) bool {
						// VM passthrough doesn't work. We will send traffic to the ClusterIP of
						// the VM service, which will have 0 Endpoints. If we generated
						// EndpointSlice's for VMs this might work.
						return !opts.To.Config().IsVM()
					},
					ExpectSuccess: func(from echo.Instance, opts echo.CallOptions) bool {
						// nolint: gosimple
						if from.Config().IsRemote() {
							if opts.To.Config().IsRemote() {
								return true
							}
							// TODO: https://github.com/solo-io/istio-sidecarless/issues/244
							return false
						}
						return true
					},
					ExpectMTLS: func(from echo.Instance, opts echo.CallOptions) bool {
						return mtlsOnExpect(from, opts)
					},

					ExpectDestinations: func(from echo.Instance, to echo.Target) echo.Instances {
						// Since we are doing passthrough, only single cluster is relevant here, as we
						// are bypassing any Istio cluster load balancing
						return match.Cluster(from.Config().Cluster).GetMatches(to.Instances())
					},
				},
			}
			RunReachability(testCases, t)
		})
}

func TestOutboundPolicyAllowAny(t *testing.T) {
	framework.NewTest(t).
		Features("traffic.ambient").
		Run(func(t framework.TestContext) {
			svcs := apps.All
			for _, svc := range svcs {
				if svc.Config().IsUncaptured() || svc.Config().HasSidecar() {
					continue
				}
				t.NewSubTestf("ALLOW_ANY %v to external service", svc.Config().Service).Run(func(t framework.TestContext) {
					// TODO use Sidecar to simulate external service (see tests/integration/pilot/mirror_test.go)
					svc.CallOrFail(t, echo.CallOptions{
						Address: "httpbin.org",
						Port:    echo.Port{Name: "http", ServicePort: 80},
						Scheme:  scheme.HTTP,
						HTTP: echo.HTTP{
							Path: "/headers",
						},
						Check: check.OK(),
					})
				})
			}
		})
}

func TestServiceEntryDNS(t *testing.T) {
	framework.NewTest(t).
		Features("traffic.ambient").
		Run(func(t framework.TestContext) {
			svcs := apps.All
			for _, svc := range svcs {
				if svc.Config().IsUncaptured() || svc.Config().HasSidecar() {
					continue
				}
				if err := t.ConfigIstio().YAML(svc.NamespaceName(), `apiVersion: networking.istio.io/v1beta1
kind: ServiceEntry
metadata:
  name: externalservice-httpbin
spec:
  exportTo:
  - .
  hosts:
  - httpbin.org
  ports:
  - name: http
    number: 80
    protocol: HTTP
  resolution: DNS`).Apply(apply.NoCleanup); err != nil {
					t.Fatal(err)
				}
				t.NewSubTestf("%v to ServiceEntry", svc.Config().Service).Run(func(t framework.TestContext) {
					// TODO use Sidecar to simulate external service (see tests/integration/pilot/mirror_test.go)
					svc.CallOrFail(t, echo.CallOptions{
						Address: "httpbin.org",
						Port:    echo.Port{Name: "http", ServicePort: 80},
						Scheme:  scheme.HTTP,
						HTTP: echo.HTTP{
							Path: "/headers",
						},
						Check: check.OK(),
					})
				})
			}
		})
}

func TestServiceEntry(t *testing.T) {
	framework.NewTest(t).
		Features("traffic.ambient").
		Run(func(t framework.TestContext) {
			testCases := []struct {
				location   v1alpha3.ServiceEntry_Location
				resolution v1alpha3.ServiceEntry_Resolution
				to         echo.Instances
			}{
				{
					location:   v1alpha3.ServiceEntry_MESH_INTERNAL,
					resolution: v1alpha3.ServiceEntry_STATIC,
					to:         apps.Mesh,
				},
				{
					location:   v1alpha3.ServiceEntry_MESH_EXTERNAL,
					resolution: v1alpha3.ServiceEntry_STATIC,
					to:         apps.MeshExternal,
				},
				// TODO dns cases
			}

			cfg := config.YAML(`
{{ $to := .To }}
apiVersion: networking.istio.io/v1beta1
kind: ServiceEntry
metadata:
  name: test-se
spec:
  hosts:
  - serviceentry.istio.io # not used
  addresses:
  - 111.111.222.222
  ports:
  - number: 80
    name: http
    protocol: HTTP
  resolution: {{.Resolution}}
  location: {{.Location}}
  endpoints:
  # we send directly to a Pod IP here. This is essentially headless
  - address: {{ (index .To.MustWorkloads 0).Address }} # TODO won't work with DNS resolution tests
    ports:
      http: {{ (.To.PortForName "http").WorkloadPort }}`).
				WithParams(param.Params{}.SetWellKnown(param.Namespace, apps.Namespace))

			for _, tc := range testCases {
				tc := tc
				t.NewSubTestf("%s %s", tc.location, tc.resolution).Run(func(t framework.TestContext) {
					echotest.
						New(t, apps.All).
						// TODO eventually we can do this for uncaptured -> l7
						FromMatch(match.Not(match.ServiceName(echo.NamespacedName{
							Name:      "uncaptured",
							Namespace: apps.Namespace,
						}))).
						ToMatch(match.AnyServiceName(tc.to.NamespacedNames())).
						Config(cfg.WithParams(param.Params{
							"Resolution": tc.resolution.String(),
							"Location":   tc.location.String(),
						})).
						Run(func(t framework.TestContext, from echo.Instance, to echo.Target) {
							if from.Config().Service == to.Config().Service {
								t.Skip("https://github.com/solo-io/istio-sidecarless/issues/263")
							}
							// TODO validate L7 processing/some headers indicating we reach the svc we wanted
							from.CallOrFail(t, echo.CallOptions{
								Address: "111.111.222.222",
								Port:    to.PortForName("http"),
							})
						})
				})
			}
		})
}

// Run runs the given reachability test cases with the context.
func RunReachability(testCases []reachability.TestCase, t framework.TestContext) {
	runTest := func(t framework.TestContext, f func(t framework.TestContext, src echo.Instance, dst echo.Instance, opt echo.CallOptions)) {
		svcs := apps.All
		for _, src := range svcs {
			src := src
			t.NewSubTestf("from %v", src.Config().Service).RunParallel(func(t framework.TestContext) {
				for _, dst := range svcs {
					dst := dst
					t.NewSubTestf("to %v", dst.Config().Service).RunParallel(func(t framework.TestContext) {
						for _, opt := range callOptions {
							opt := opt
							t.NewSubTestf("%v", opt.Scheme).RunParallel(func(t framework.TestContext) {
								opt = opt.DeepCopy()
								opt.To = dst
								opt.Check = check.OK()
								f(t, src, dst, opt)
							})
						}
					})
				}
			})
		}
	}
	for _, c := range testCases {
		// Create a copy to avoid races, as tests are run in parallel
		c := c
		testName := strings.TrimSuffix(c.ConfigFile, filepath.Ext(c.ConfigFile))
		t.NewSubTest(testName).Run(func(t framework.TestContext) {
			// Apply the policy.
			cfg := t.ConfigIstio().File(c.Namespace.Name(), filepath.Join("testdata", c.ConfigFile))
			retry.UntilSuccessOrFail(t, func() error {
				t.Logf("[%s] [%v] Apply config %s", testName, time.Now(), c.ConfigFile)
				// TODO(https://github.com/istio/istio/issues/20460) We shouldn't need a retry loop
				return cfg.Apply(apply.Wait)
			})
			runTest(t, func(t framework.TestContext, src echo.Instance, dst echo.Instance, opt echo.CallOptions) {
				expectSuccess := c.ExpectSuccess(src, opt)
				expectMTLS := c.ExpectMTLS(src, opt)

				var tpe string
				if expectSuccess {
					tpe = "positive"
					opt.Check = check.And(
						check.OK(),
						check.ReachedTargetClusters(t))
					if expectMTLS {
						// TODO(https://github.com/solo-io/istio-sidecarless/issues/150)
						_ = expectMTLS
					}
				} else {
					tpe = "negative"
					opt.Check = check.NotOK()
				}
				t.Logf("expected result: %v", tpe)

				include := c.Include
				if include == nil {
					include = func(_ echo.Instance, _ echo.CallOptions) bool { return true }
				}
				if !include(src, opt) {
					t.Skip("excluded")
				}
				src.CallOrFail(t, opt)
			})
		})
	}
}

func TestIngress(t *testing.T) {
	runIngressTest(t, func(t framework.TestContext, src ingress.Instance, dst echo.Instance, opt echo.CallOptions) {
		if opt.Scheme != scheme.HTTP {
			return
		}
		t.ConfigIstio().Eval(apps.Namespace.Name(), map[string]string{
			"Destination": dst.Config().Service,
		}, `apiVersion: networking.istio.io/v1alpha3
kind: Gateway
metadata:
  name: gateway
spec:
  selector:
    istio: ingressgateway
  servers:
  - port:
      number: 80
      name: http
      protocol: HTTP
    hosts: ["*"]
---
apiVersion: networking.istio.io/v1alpha3
kind: VirtualService
metadata:
  name: route
spec:
  gateways:
  - gateway
  hosts:
  - "*"
  http:
  - route:
    - destination:
        host: "{{.Destination}}"
`).ApplyOrFail(t)
		src.CallOrFail(t, opt)
	})
}

var CheckDeny = check.Or(
	check.ErrorContains("rpc error: code = PermissionDenied"), // gRPC
	check.ErrorContains("EOF"),                                // TCP
	check.NoErrorAndStatus(http.StatusForbidden),              // HTTP
	check.NoErrorAndStatus(http.StatusServiceUnavailable),     // HTTP client, TCP server
)

func runTest(t *testing.T, f func(t framework.TestContext, src echo.Instance, dst echo.Instance, opt echo.CallOptions)) {
	framework.NewTest(t).Features("traffic.ambient").Run(func(t framework.TestContext) {
		runTestContext(t, f)
	})
}

func runTestContext(t framework.TestContext, f func(t framework.TestContext, src echo.Instance, dst echo.Instance, opt echo.CallOptions)) {
	svcs := apps.All
	for _, src := range svcs {
		t.NewSubTestf("from %v", src.Config().Service).Run(func(t framework.TestContext) {
			for _, dst := range svcs {
				t.NewSubTestf("to %v", dst.Config().Service).Run(func(t framework.TestContext) {
					for _, opt := range callOptions {
						src, dst, opt := src, dst, opt
						t.NewSubTestf("%v", opt.Scheme).Run(func(t framework.TestContext) {
							opt = opt.DeepCopy()
							opt.To = dst
							opt.Check = check.OK()
							f(t, src, dst, opt)
						})
					}
				})
			}
		})
	}
}

func runIngressTest(t *testing.T, f func(t framework.TestContext, src ingress.Instance, dst echo.Instance, opt echo.CallOptions)) {
	framework.NewTest(t).Features("traffic.ambient").Run(func(t framework.TestContext) {
		svcs := apps.All
		for _, dst := range svcs {
			t.NewSubTestf("to %v", dst.Config().Service).Run(func(t framework.TestContext) {
				dst := dst
				opt := echo.CallOptions{
					Port:    echo.Port{Name: "http"},
					Scheme:  scheme.HTTP,
					Count:   5,
					Timeout: time.Second * 2,
					Check:   check.OK(),
					To:      dst,
				}
				f(t, istio.DefaultIngressOrFail(t, t), dst, opt)
			})
		}
	})
}

func TestL7Telemetry(t *testing.T) {
	framework.NewTest(t).
		Features("observability.telemetry.stats.prometheus.ambient").
		Run(func(tc framework.TestContext) {
			// ensure that some traffic from each captured workload is
			// sent to each remote PEP. This will likely have happened in
			// the other tests (without the teardown), but we want to make
			// sure that some traffic is seen. This test will not validate
			// exact traffic counts, but rather focus on validating that
			// the telemetry is being created and collected properly.
			for _, src := range apps.Captured {
				for _, dst := range apps.Remote {
					tc.NewSubTestf("from %q to %q", src.Config().Service, dst.Config().Service).Run(func(stc framework.TestContext) {
						localDst := dst
						localSrc := src
						opt := echo.CallOptions{
							Port:    echo.Port{Name: "http"},
							Scheme:  scheme.HTTP,
							Count:   5,
							Timeout: time.Second,
							Check:   check.OK(),
							To:      localDst,
						}
						// allow for delay between prometheus pulls from target pod
						// pulls should happen every 15s, so timeout if not found within 30s

						query := buildQuery(localSrc, localDst)
						stc.Logf("prometheus query: %#v", query)
						err := retry.Until(func() bool {
							stc.Logf("sending call from %q to %q", deployName(localSrc), localDst.Config().Service)
							localSrc.CallOrFail(stc, opt)
							reqs, err := prom.QuerySum(localSrc.Config().Cluster, query)
							if err != nil {
								stc.Logf("could not query for traffic from %q to %q: %v", deployName(localSrc), localDst.Config().Service, err)
								return false
							}
							if reqs == 0.0 {
								stc.Logf("found zero-valued sum for traffic from %q to %q: %v", deployName(localSrc), localDst.Config().Service, err)
								return false
							}
							return true
						}, retry.Timeout(30*time.Second), retry.BackoffDelay(1*time.Second))
						if err != nil {
							util.PromDiff(t, prom, localSrc.Config().Cluster, query)
							stc.Errorf("could not validate L7 telemetry for %q to %q: %v", deployName(localSrc), localDst.Config().Service, err)
						}
					})
				}
			}
		})
}

func buildQuery(src, dst echo.Instance) prometheus.Query {
	query := prometheus.Query{}

	srcns := src.NamespaceName()
	destns := dst.NamespaceName()

	labels := map[string]string{
		"reporter":                       "destination",
		"request_protocol":               "http",
		"response_code":                  "200",
		"response_flags":                 "-",
		"connection_security_policy":     "mutual_tls",
		"destination_canonical_service":  dst.ServiceName(),
		"destination_canonical_revision": dst.Config().Version,
		"destination_service":            fmt.Sprintf("%s.%s.svc.cluster.local", dst.Config().Service, destns),
		"destination_principal":          "spiffe://" + dst.Config().ServiceAccountName(),
		"destination_service_name":       dst.Config().Service,
		"destination_workload":           deployName(dst),
		"destination_workload_namespace": destns,
		"destination_service_namespace":  destns,
		"source_canonical_service":       src.ServiceName(),
		"source_canonical_revision":      src.Config().Version,
		"source_principal":               "spiffe://" + src.Config().ServiceAccountName(),
		"source_workload":                deployName(src),
		"source_workload_namespace":      srcns,
	}

	query.Metric = "istio_requests_total"
	query.Labels = labels

	return query
}

func deployName(inst echo.Instance) string {
	return inst.ServiceName() + "-" + inst.Config().Version
}

func TestMetadataServer(t *testing.T) {
	framework.NewTest(t).Features("traffic.ambient").Run(func(t framework.TestContext) {
		ver, _ := t.Clusters().Default().GetKubernetesVersion()
		if !strings.Contains(ver.GitVersion, "-gke") {
			t.Skip("requires GKE cluster")
		}
		svcs := apps.All
		for _, src := range svcs {
			src := src
			t.NewSubTestf("from %v", src.Config().Service).Run(func(t framework.TestContext) {
				// curl -H "Metadata-Flavor: Google" 169.254.169.254/computeMetadata/v1/instance/service-accounts/default/identity
				opts := echo.CallOptions{
					Address: "169.254.169.254",
					Port:    echo.Port{ServicePort: 80},
					Scheme:  scheme.HTTP,
					HTTP: echo.HTTP{
						// TODO: detect which platform?
						Headers: headers.New().With("Metadata-Flavor", "Google").Build(),
						Path:    "/computeMetadata/v1/instance/service-accounts/default/identity",
					},
					// Test that we see our own identity -- not the uproxy (istio-system/uproxy).
					// TODO: if the test SA actually had workload identity enabled the result is probably different
					Check: check.BodyContains(fmt.Sprintf(`Your Kubernetes service account (%s/%s)`, src.NamespaceName(), src.Config().AccountName())),
				}
				src.CallOrFail(t, opts)
			})
		}
	})
}

func TestDirect(t *testing.T) {
	framework.NewTest(t).Features("traffic.ambient").Run(func(t framework.TestContext) {
		t.NewSubTest("PEP").Run(func(t framework.TestContext) {
			c := common.NewCaller()
			cert, err := istio.CreateCertificate(t, i, apps.Captured.ServiceName(), apps.Namespace.Name())
			if err != nil {
				t.Fatal(err)
			}
			hb := echo.HBONE{
				Address:            apps.RemotePEP.Inbound(),
				Headers:            nil,
				Cert:               string(cert.ClientCert),
				Key:                string(cert.Key),
				CaCert:             string(cert.RootCert),
				InsecureSkipVerify: true,
			}
			run := func(name string, options echo.CallOptions) {
				t.NewSubTest(name).Run(func(t framework.TestContext) {
					_, err := c.CallEcho(nil, options)
					if err != nil {
						t.Fatal(err)
					}
				})
			}
			const UnknownRoute = "round trip failed: 404 Not Found"
			run("named destination", echo.CallOptions{
				To:    apps.Remote,
				Count: 1,
				Port:  echo.Port{Name: ports.HTTP},
				HBONE: hb,
				// TODO(https://github.com/solo-io/istio-sidecarless/issues/269)
				Check: check.ErrorContains(UnknownRoute),
			})
			run("VIP destination", echo.CallOptions{
				To:      apps.Remote,
				Count:   1,
				Address: apps.Remote[0].Address(),
				Port:    echo.Port{Name: ports.HTTP},
				HBONE:   hb,
				Check:   check.OK(),
			})
			run("VIP destination, unknown port", echo.CallOptions{
				To:      apps.Remote,
				Count:   1,
				Address: apps.Remote[0].Address(),
				Port:    echo.Port{ServicePort: 12345},
				Scheme:  scheme.HTTP,
				HBONE:   hb,
				Check:   check.ErrorContains(UnknownRoute),
			})
			run("Pod IP destination", echo.CallOptions{
				To:      apps.Remote,
				Count:   1,
				Address: apps.Remote[0].WorkloadsOrFail(t)[0].Address(),
				Port:    echo.Port{ServicePort: ports.All().MustForName(ports.HTTP).WorkloadPort},
				Scheme:  scheme.HTTP,
				HBONE:   hb,
				Check:   check.OK(),
			})
			run("Unserved VIP destination", echo.CallOptions{
				To:      apps.Captured,
				Count:   1,
				Address: apps.Captured[0].Address(),
				Port:    echo.Port{ServicePort: ports.All().MustForName(ports.HTTP).ServicePort},
				Scheme:  scheme.HTTP,
				HBONE:   hb,
				Check:   check.ErrorContains(UnknownRoute),
			})
			run("Unserved pod destination", echo.CallOptions{
				To:      apps.Captured,
				Count:   1,
				Address: apps.Captured[0].WorkloadsOrFail(t)[0].Address(),
				Port:    echo.Port{ServicePort: ports.All().MustForName(ports.HTTP).ServicePort},
				Scheme:  scheme.HTTP,
				HBONE:   hb,
				Check:   check.ErrorContains(UnknownRoute),
			})
		})
	})
}

type PEP struct {
	Namespace      string
	ServiceAccount string
}
