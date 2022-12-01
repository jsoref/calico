// Copyright (c) 2020-2021 Tigera, Inc. All rights reserved.
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

//go:build fvtests

package fv_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"

	"github.com/projectcalico/calico/felix/fv/connectivity"
	"github.com/projectcalico/calico/felix/fv/utils"

	log "github.com/sirupsen/logrus"
	"github.com/vishvananda/netlink"

	api "github.com/projectcalico/api/pkg/apis/projectcalico/v3"
	"github.com/projectcalico/api/pkg/lib/numorstring"

	"github.com/projectcalico/calico/libcalico-go/lib/apiconfig"
	client "github.com/projectcalico/calico/libcalico-go/lib/clientv3"
	cerrors "github.com/projectcalico/calico/libcalico-go/lib/errors"
	"github.com/projectcalico/calico/libcalico-go/lib/options"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/projectcalico/calico/felix/fv/containers"
	"github.com/projectcalico/calico/felix/fv/infrastructure"
	"github.com/projectcalico/calico/felix/fv/workload"
)

var _ = infrastructure.DatastoreDescribe("_BPF-SAFE_ IPIP topology before adding host IPs to IP sets", []apiconfig.DatastoreType{apiconfig.EtcdV3, apiconfig.Kubernetes}, func(getInfra infrastructure.InfraFactory) {

	var (
		bpfEnabled = os.Getenv("FELIX_FV_ENABLE_BPF") == "true"
		infra      infrastructure.DatastoreInfra
		felixes    []*infrastructure.Felix
		client     client.Interface
		w          [2]*workload.Workload
		hostW      [2]*workload.Workload
		cc         *connectivity.Checker
	)

	BeforeEach(func() {
		infra = getInfra()
		if BPFMode() && getDataStoreType(infra) == "etcdv3" {
			Skip("Skipping BPF test for etcdv3 backend.")
		}
		felixes, client = infrastructure.StartNNodeTopology(2, infrastructure.DefaultTopologyOptions(), infra)

		// Install a default profile that allows all ingress and egress, in the absence of any Policy.
		infra.AddDefaultAllow()

		// Wait until the tunl0 device appears; it is created when felix inserts the ipip module
		// into the kernel.
		Eventually(func() error {
			links, err := netlink.LinkList()
			if err != nil {
				return err
			}
			for _, link := range links {
				if link.Attrs().Name == "tunl0" {
					return nil
				}
			}
			return errors.New("tunl0 wasn't auto-created")
		}).Should(BeNil())

		// Create workloads, using that profile.  One on each "host".
		for ii := range w {
			wIP := fmt.Sprintf("10.65.%d.2", ii)
			wName := fmt.Sprintf("w%d", ii)
			w[ii] = workload.Run(felixes[ii], wName, "default", wIP, "8055", "tcp")
			w[ii].ConfigureInInfra(infra)

			hostW[ii] = workload.Run(felixes[ii], fmt.Sprintf("host%d", ii), "", felixes[ii].IP, "8055", "tcp")
		}

		if bpfEnabled {
			ensureAllNodesBPFProgramsAttached(felixes)
		}

		cc = &connectivity.Checker{}
	})

	AfterEach(func() {
		if CurrentGinkgoTestDescription().Failed {
			for _, felix := range felixes {
				felix.Exec("iptables-save", "-c")
				felix.Exec("ipset", "list")
				felix.Exec("ip", "r")
				felix.Exec("ip", "a")
				if BPFMode() {
					felix.Exec("calico-bpf", "policy", "dump", "eth0", "all")
				}
			}
		}

		for _, wl := range w {
			wl.Stop()
		}
		for _, wl := range hostW {
			wl.Stop()
		}
		for _, felix := range felixes {
			felix.Stop()
		}

		if CurrentGinkgoTestDescription().Failed {
			infra.DumpErrorData()
		}
		infra.Stop()
	})

	It("should use the --random-fully flag in the MASQUERADE rules", func() {
		for _, felix := range felixes {
			Eventually(func() string {
				out, _ := felix.ExecOutput("iptables-save", "-c")
				return out
			}, "10s", "100ms").Should(ContainSubstring("--random-fully"))
		}
	})

	It("should have workload to workload connectivity", func() {
		cc.ExpectSome(w[0], w[1])
		cc.ExpectSome(w[1], w[0])
		cc.CheckConnectivity()
	})

	It("should have host to workload connectivity", func() {
		cc.ExpectSome(felixes[0], w[1])
		cc.ExpectSome(felixes[0], w[0])
		cc.CheckConnectivity()
	})

	It("should have host to host connectivity", func() {
		cc.ExpectSome(felixes[0], hostW[1])
		cc.ExpectSome(felixes[1], hostW[0])
		cc.CheckConnectivity()
	})

	Context("with host protection policy in place", func() {
		BeforeEach(func() {
			// Make sure our new host endpoints don't cut felix off from the datastore.
			err := infra.AddAllowToDatastore("host-endpoint=='true'")
			Expect(err).NotTo(HaveOccurred())

			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()

			for _, f := range felixes {
				hep := api.NewHostEndpoint()
				hep.Name = "eth0-" + f.Name
				hep.Labels = map[string]string{
					"host-endpoint": "true",
				}
				hep.Spec.Node = f.Hostname
				hep.Spec.ExpectedIPs = []string{f.IP}
				_, err := client.HostEndpoints().Create(ctx, hep, options.SetOptions{})
				Expect(err).NotTo(HaveOccurred())
			}
		})

		It("should have workload connectivity but not host connectivity", func() {
			// Host endpoints (with no policies) block host-host traffic due to default drop.
			cc.ExpectNone(felixes[0], hostW[1])
			cc.ExpectNone(felixes[1], hostW[0])
			// But the rules to allow IPIP between our hosts let the workload traffic through.
			cc.ExpectSome(w[0], w[1])
			cc.ExpectSome(w[1], w[0])
			cc.CheckConnectivity()
		})
	})

	Context("with all-interfaces host protection policy in place", func() {
		BeforeEach(func() {
			// Make sure our new host endpoints don't cut felix off from the datastore.
			err := infra.AddAllowToDatastore("host-endpoint=='true'")
			Expect(err).NotTo(HaveOccurred())

			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()

			// Create host endpoints for each node.
			for _, f := range felixes {
				hep := api.NewHostEndpoint()
				hep.Name = "all-interfaces-" + f.Name
				hep.Labels = map[string]string{
					"host-endpoint": "true",
					"hostname":      f.Hostname,
				}
				hep.Spec.Node = f.Hostname
				hep.Spec.ExpectedIPs = []string{f.IP}
				hep.Spec.InterfaceName = "*"
				_, err := client.HostEndpoints().Create(ctx, hep, options.SetOptions{})
				Expect(err).NotTo(HaveOccurred())
			}
		})

		It("should block host-to-host traffic in the absence of policy allowing it", func() {
			cc.ExpectNone(felixes[0], hostW[1])
			cc.ExpectNone(felixes[1], hostW[0])
			cc.ExpectSome(w[0], w[1])
			cc.ExpectSome(w[1], w[0])
			cc.CheckConnectivity()
		})

		It("should allow host-to-own-pod traffic in the absence of policy allowing it but not host to other-pods", func() {
			cc.ExpectSome(felixes[0], w[0])
			cc.ExpectSome(felixes[1], w[1])
			cc.ExpectNone(felixes[0], w[1])
			cc.ExpectNone(felixes[1], w[0])
			cc.CheckConnectivity()
		})

		It("should allow felixes[0] to reach felixes[1] if ingress and egress policies are in place", func() {
			// Create a policy selecting felix[1] that allows egress.
			policy := api.NewGlobalNetworkPolicy()
			policy.Name = "f0-egress"
			policy.Spec.Egress = []api.Rule{{Action: api.Allow}}
			policy.Spec.Selector = fmt.Sprintf("hostname == '%s'", felixes[0].Hostname)
			_, err := client.GlobalNetworkPolicies().Create(utils.Ctx, policy, utils.NoOptions)
			Expect(err).NotTo(HaveOccurred())

			// But there is no policy allowing ingress into felix[1].
			cc.ExpectNone(felixes[0], hostW[1])
			cc.ExpectNone(felixes[1], hostW[0])

			// Workload connectivity is unchanged.
			cc.ExpectSome(w[0], w[1])
			cc.ExpectSome(w[1], w[0])
			cc.CheckConnectivity()
			cc.ResetExpectations()

			// Now add a policy selecting felix[1] that allows ingress.
			policy = api.NewGlobalNetworkPolicy()
			policy.Name = "f1-ingress"
			policy.Spec.Ingress = []api.Rule{{Action: api.Allow}}
			policy.Spec.Selector = fmt.Sprintf("hostname == '%s'", felixes[1].Hostname)
			_, err = client.GlobalNetworkPolicies().Create(utils.Ctx, policy, utils.NoOptions)
			Expect(err).NotTo(HaveOccurred())

			// Now felixes[0] can reach felixes[1].
			cc.ExpectSome(felixes[0], hostW[1])
			cc.ExpectNone(felixes[1], hostW[0])

			// Workload connectivity is unchanged.
			cc.ExpectSome(w[0], w[1])
			cc.ExpectSome(w[1], w[0])
			cc.CheckConnectivity()
		})

		Context("with policy allowing port 8055", func() {
			BeforeEach(func() {
				tcp := numorstring.ProtocolFromString("tcp")
				udp := numorstring.ProtocolFromString("udp")
				p8055 := numorstring.SinglePort(8055)
				policy := api.NewGlobalNetworkPolicy()
				policy.Name = "allow-8055"
				policy.Spec.Ingress = []api.Rule{
					{
						Protocol: &udp,
						Destination: api.EntityRule{
							Ports: []numorstring.Port{p8055},
						},
						Action: api.Allow,
					},
					{
						Protocol: &tcp,
						Destination: api.EntityRule{
							Ports: []numorstring.Port{p8055},
						},
						Action: api.Allow,
					},
				}
				policy.Spec.Egress = []api.Rule{
					{
						Protocol: &udp,
						Destination: api.EntityRule{
							Ports: []numorstring.Port{p8055},
						},
						Action: api.Allow,
					},
					{
						Protocol: &tcp,
						Destination: api.EntityRule{
							Ports: []numorstring.Port{p8055},
						},
						Action: api.Allow,
					},
				}
				policy.Spec.Selector = fmt.Sprintf("has(host-endpoint)")
				_, err := client.GlobalNetworkPolicies().Create(utils.Ctx, policy, utils.NoOptions)
				Expect(err).NotTo(HaveOccurred())
			})

			// Please take care if adding other connectivity checks into this case, to
			// avoid those other checks setting up conntrack state that allows the
			// existing case to pass for a different reason.
			It("allows host0 to remote Calico-networked workload via service IP", func() {
				// Allocate a service IP.
				serviceIP := "10.101.0.11"
				port := 8055
				tgtPort := 8055

				createK8sServiceWithoutKubeProxy(infra, felixes[0], w[1], "test-svc", serviceIP, w[1].IP, port, tgtPort, "OUTPUT")
				// Expect to connect to the service IP.
				cc.ExpectSome(felixes[0], connectivity.TargetIP(serviceIP), uint16(port))
				cc.CheckConnectivity()
			})
		})
	})

	Context("after removing BGP address from nodes", func() {
		// Simulate having a host send IPIP traffic from an unknown source, should get blocked.
		BeforeEach(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			if bpfEnabled {
				infra.RemoveNodeAddresses(felixes[0])
			} else {
				for _, f := range felixes {
					infra.RemoveNodeAddresses(f)
				}
			}

			listOptions := options.ListOptions{}
			if bpfEnabled {
				listOptions.Name = felixes[0].Hostname
			}
			l, err := client.Nodes().List(ctx, listOptions)
			Expect(err).NotTo(HaveOccurred())
			for _, node := range l.Items {
				node.Spec.BGP = nil
				_, err := client.Nodes().Update(ctx, &node, options.SetOptions{})
				Expect(err).NotTo(HaveOccurred())
			}

			if bpfEnabled {
				Eventually(felixes[1].NumTCBPFProgsEth0, "5s", "200ms").Should(Equal(2))
			} else {
				for _, f := range felixes {
					// Removing the BGP config triggers a Felix restart and Felix has a 2s timer during
					// a config restart to ensure that it doesn't tight loop.  Wait for the ipset to be
					// updated as a signal that Felix has restarted.
					Eventually(func() int {
						return getNumIPSetMembers(f.Container, "cali40all-hosts-net")
					}, "5s", "200ms").Should(BeZero())
				}
			}
		})

		It("should have no workload to workload connectivity", func() {
			cc.ExpectNone(w[0], w[1])
			cc.ExpectNone(w[1], w[0])
			cc.CheckConnectivity()
		})
	})

	Context("external nodes configured", func() {

		var externalClient *containers.Container

		BeforeEach(func() {
			externalClient = infrastructure.RunExtClient("ext-client")

			updateConfig := func(addr string) {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				c, err := client.FelixConfigurations().Get(ctx, "default", options.GetOptions{})
				if err != nil {
					// Create the default config if it doesn't already exist.
					if _, ok := err.(cerrors.ErrorResourceDoesNotExist); ok {
						c = api.NewFelixConfiguration()
						c.Name = "default"
						c, err = client.FelixConfigurations().Create(ctx, c, options.SetOptions{})
						Expect(err).NotTo(HaveOccurred())
					} else {
						Expect(err).NotTo(HaveOccurred())
					}
				}
				c.Spec.ExternalNodesCIDRList = &[]string{addr}
				log.WithFields(log.Fields{"felixconfiguration": c, "adding Addr": addr}).Info("Updating FelixConfiguration ")
				_, err = client.FelixConfigurations().Update(ctx, c, options.SetOptions{})
				Expect(err).NotTo(HaveOccurred())
			}

			updateConfig(externalClient.IP)

			// Wait for the config to take
			for _, f := range felixes {
				Eventually(func() int {
					return getNumIPSetMembers(f.Container, "cali40all-hosts-net")
				}, "5s", "200ms").Should(Equal(3))
			}

			Eventually(func() error {
				err := externalClient.ExecMayFail("ip", "tunnel", "add", "tunl0", "mode", "ipip")
				if err != nil && strings.Contains(err.Error(), "SIOCADDTUNNEL: File exists") {
					return nil
				}
				return err
			}).Should(Succeed())

			externalClient.Exec("ip", "link", "set", "tunl0", "up")
			externalClient.Exec("ip", "addr", "add", "dev", "tunl0", "10.65.222.1")
			externalClient.Exec("ip", "route", "add", "10.65.0.0/24", "via",
				felixes[0].IP, "dev", "tunl0", "onlink")

			felixes[0].Exec("ip", "route", "add", "10.65.222.1", "via",
				externalClient.IP, "dev", "tunl0", "onlink")
		})

		JustAfterEach(func() {
			if CurrentGinkgoTestDescription().Failed {
				externalClient.Exec("ip", "r")
				externalClient.Exec("ip", "l")
				externalClient.Exec("ip", "a")
			}

		})
		AfterEach(func() {
			externalClient.Stop()
		})

		It("should have all-hosts-net ipset configured with the external hosts and workloads connect", func() {
			cc.ExpectSome(externalClient, w[0])
			cc.CheckConnectivity()
		})
	})
})

func getNumIPSetMembers(c *containers.Container, ipSetName string) int {
	return getIPSetCounts(c)[ipSetName]
}

func getIPSetCounts(c *containers.Container) map[string]int {
	ipsetsOutput, err := c.ExecOutput("ipset", "list")
	Expect(err).NotTo(HaveOccurred())
	numMembers := map[string]int{}
	currentName := ""
	membersSeen := false
	log.WithField("ipsets", ipsetsOutput).Info("IP sets state")
	for _, line := range strings.Split(ipsetsOutput, "\n") {
		log.WithField("line", line).Debug("Parsing line")
		if strings.HasPrefix(line, "Name:") {
			currentName = strings.Split(line, " ")[1]
			membersSeen = false
		} else if strings.HasPrefix(line, "Members:") {
			membersSeen = true
		} else if membersSeen && len(strings.TrimSpace(line)) > 0 {
			log.Debugf("IP set %s has member %s", currentName, line)
			numMembers[currentName]++
		}
	}
	return numMembers
}

func createK8sServiceWithoutKubeProxy(infra infrastructure.DatastoreInfra, felix *infrastructure.Felix, w *workload.Workload, svcName, serviceIP, targetIP string, port, tgtPort int, chain string) {
	if BPFMode() {
		k8sClient := infra.(*infrastructure.K8sDatastoreInfra).K8sClient
		testSvc := k8sService(svcName, serviceIP, w, port, tgtPort, 0, "tcp")
		testSvcNamespace := testSvc.ObjectMeta.Namespace
		_, err := k8sClient.CoreV1().Services(testSvcNamespace).Create(context.Background(), testSvc, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		Eventually(k8sGetEpsForServiceFunc(k8sClient, testSvc), "10s").Should(HaveLen(1),
			"Service endpoints didn't get created? Is controller-manager happy?")
	}
	felix.ProgramIptablesDNAT(serviceIP, targetIP, chain)
}

func getDataStoreType(infra infrastructure.DatastoreInfra) string {
	switch infra.(type) {
	case *infrastructure.K8sDatastoreInfra:
		return "kubernetes"
	case *infrastructure.EtcdDatastoreInfra:
		return "etcdv3"
	default:
		return "kubernetes"
	}
}
