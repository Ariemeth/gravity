/*
Copyright 2020 Gravitational, Inc.

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

package cluster

import (
	"bytes"
	"errors"
	"fmt"
	"github.com/gravitational/gravity/lib/utils"
	"os/exec"
	"sort"
	"strconv"
	"strings"

	"github.com/gravitational/gravity/lib/constants"
	"github.com/gravitational/gravity/lib/defaults"
	"github.com/gravitational/gravity/lib/loc"
	"github.com/gravitational/gravity/lib/pack"
	"github.com/gravitational/gravity/lib/storage"
	"github.com/gravitational/gravity/lib/update"
	libphase "github.com/gravitational/gravity/lib/update/cluster/phases"

	"github.com/coreos/go-semver/semver"
	"github.com/gravitational/rigging"
	"github.com/gravitational/trace"
	log "github.com/sirupsen/logrus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

func (r phaseBuilder) init(leadMaster storage.Server) *update.Phase {
	phase := update.RootPhase(update.Phase{
		ID:          "init",
		Executor:    updateInit,
		Description: "Initialize update operation",
		Data: &storage.OperationPhaseData{
			Package:          &r.updateApp.Package,
			ExecServer:       &leadMaster,
			InstalledPackage: &r.installedApp.Package,
			Update: &storage.UpdateOperationData{
				Servers: r.servers,
			},
		},
	})
	return &phase
}

func (r phaseBuilder) checks() *update.Phase {
	phase := update.RootPhase(update.Phase{
		ID:          "checks",
		Executor:    updateChecks,
		Description: "Run preflight checks",
		Data: &storage.OperationPhaseData{
			Package:          &r.updateApp.Package,
			InstalledPackage: &r.installedApp.Package,
		},
	})

	return &phase
}

func (r phaseBuilder) hasSELinuxPhase() bool {
	for _, server := range r.servers {
		if server.SELinux {
			return true
		}
	}
	return false
}

func (r phaseBuilder) bootstrapSELinux() *update.Phase {
	root := update.RootPhase(update.Phase{
		ID:          "selinux-bootstrap",
		Description: "Configure SELinux on nodes",
	})

	for i, server := range r.servers {
		if !server.SELinux {
			continue
		}
		root.AddParallel(update.Phase{
			ID:          root.ChildLiteral(server.Hostname),
			Executor:    updateBootstrapSELinux,
			Description: fmt.Sprintf("Configure SELinux on node %q", server.Hostname),
			Data: &storage.OperationPhaseData{
				ExecServer:       &r.servers[i].Server,
				Package:          &r.updateApp.Package,
				InstalledPackage: &r.installedApp.Package,
				Update: &storage.UpdateOperationData{
					Servers: []storage.UpdateServer{server},
				},
			},
		})
	}
	return &root
}

func (r phaseBuilder) bootstrap() *update.Phase {
	root := update.RootPhase(update.Phase{
		ID:          "bootstrap",
		Description: "Bootstrap update operation on nodes",
	})

	for i, server := range r.servers {
		root.AddParallel(update.Phase{
			ID:          root.ChildLiteral(server.Hostname),
			Executor:    updateBootstrap,
			Description: fmt.Sprintf("Bootstrap node %q", server.Hostname),
			Data: &storage.OperationPhaseData{
				ExecServer:       &r.servers[i].Server,
				Package:          &r.updateApp.Package,
				InstalledPackage: &r.installedApp.Package,
				Update: &storage.UpdateOperationData{
					Servers: []storage.UpdateServer{server},
				},
			},
		})
	}
	return &root
}

func (r phaseBuilder) preUpdate() *update.Phase {
	phase := update.RootPhase(update.Phase{
		ID:          "pre-update",
		Description: "Run pre-update application hook",
		Executor:    preUpdate,
		Data: &storage.OperationPhaseData{
			Package: &r.updateApp.Package,
		},
	})
	return &phase
}

func (r phaseBuilder) corednsPhase(leadMaster storage.Server) *update.Phase {
	phase := update.RootPhase(update.Phase{
		ID:          "coredns",
		Description: "Provision CoreDNS resources",
		Executor:    coredns,
		Data: &storage.OperationPhaseData{
			Server: &leadMaster,
		},
	})
	return &phase
}

func (r phaseBuilder) app(updates []loc.Locator) *update.Phase {
	root := update.RootPhase(update.Phase{
		ID:          "app",
		Description: "Update installed application",
	})

	for i, loc := range updates {
		root.AddParallel(update.Phase{
			ID:          loc.Name,
			Executor:    updateApp,
			Description: fmt.Sprintf("Update application %q to %v", loc.Name, loc.Version),
			Data: &storage.OperationPhaseData{
				Package: &updates[i],
				Values:  r.operation.Vars().Values,
			},
		})
	}
	return &root
}

// migration constructs a migration phase based on the plan params.
//
// If there are no migrations to perform, returns nil.
func (r phaseBuilder) migration(leadMaster storage.Server) *update.Phase {
	root := update.RootPhase(update.Phase{
		ID:          "migration",
		Description: "Perform system database migration",
	})

	var subphases []update.Phase

	// do we need to migrate links to trusted clusters?
	if len(r.links) != 0 && len(r.trustedClusters) == 0 {
		subphases = append(subphases, update.Phase{
			ID:          root.ChildLiteral("links"),
			Description: "Migrate remote Gravity Hub links to trusted clusters",
			Executor:    migrateLinks,
		})
	}

	// Update / reset the labels during upgrade
	subphases = append(subphases, update.Phase{
		ID:          root.ChildLiteral("labels"),
		Description: "Update node labels",
		Executor:    updateLabels,
	})

	// migrate roles
	if libphase.NeedMigrateRoles(r.roles) {
		subphases = append(subphases, update.Phase{
			ID:          root.ChildLiteral("roles"),
			Description: "Migrate cluster roles to a new format",
			Executor:    migrateRoles,
			Data: &storage.OperationPhaseData{
				ExecServer: &leadMaster,
			},
		})
	}

	// no migrations needed
	if len(subphases) == 0 {
		return nil
	}

	root.AddParallel(subphases...)
	return &root
}

// Only applicable for 5.3.0 -> 5.3.2
// We need to update the CoreDNS app before doing rolling restarts, because the new planet will not have embedded
// coredns, and will instead point to the kube-dns service on startup. Updating the app will deploy coredns as pods.
// TODO(knisbet) remove when 5.3.2 is no longer supported as an upgrade path
func (r phaseBuilder) earlyDNSApp(locator loc.Locator) *update.Phase {
	phase := update.Phase{
		ID:       locator.Name,
		Executor: updateApp,
		Description: fmt.Sprintf(
			"Update system application %q to %v", locator.Name, locator.Version),
		Data: &storage.OperationPhaseData{
			Package: &locator,
		},
	}
	return &phase
}

// config returns phase that pulls system configuration on provided nodes
func (r phaseBuilder) config(nodes []storage.Server) *update.Phase {
	root := update.RootPhase(update.Phase{
		ID:          "config",
		Description: "Update system configuration on nodes",
	})
	for i, node := range nodes {
		root.AddParallel(update.Phase{
			ID:       root.ChildLiteral(node.Hostname),
			Executor: config,
			Description: fmt.Sprintf("Update system configuration on node %q",
				node.Hostname),
			Data: &storage.OperationPhaseData{
				Server: &nodes[i],
			},
		})
	}
	return &root
}

// openEBS returns phase that creates OpenEBS configuration in the cluster.
func (r phaseBuilder) openEBS(leadMaster storage.UpdateServer) *update.Phase {
	phase := update.RootPhase(update.Phase{
		ID:          "openebs",
		Executor:    openebs,
		Description: "Create OpenEBS configuration",
		Data: &storage.OperationPhaseData{
			ExecServer: &leadMaster.Server,
		},
	})
	return &phase
}

func (r phaseBuilder) openEBSUpgrade(leadMaster storage.UpdateServer, root *update.Phase) error {

	var out bytes.Buffer

	// Upgrade pools
	// cstor-pool-y7ru-dcfb9b955-lqdtd                                   3/3     Running     3          144m   app=cstor-pool,openebs.io/cstor-pool=cstor-pool-y7ru,openebs.io/storage-pool-claim=cstor-pool,openebs.io/version=2.2.0,pod-template-hash=dcfb9b955
	// TODO use kubectl.Command("get","pods","--field-selector","status.phase=Running","--selector=app","cstor-volAndVer-manager,openebs\.io/storage-class=openebs-cstor","-n","openebs","-o","jsonpath='{.items[*].metadata.labels.openebs\.io/persistent-volAndVer}{" "}{.items[*].metadata.labels.openebs\.io/version}'")
	if err := utils.Exec(exec.Command("/bin/bash", "-c", "kubectl get pods --field-selector=status.phase=Running  --selector=app=cstor-pool  -nopenebs -o  jsonpath='{.items[*].metadata.labels.openebs\\.io/storage-pool-claim}{\" \"}{.items[*].metadata.labels.openebs\\.io/version}'"), &out); err != nil {
		fmt.Printf("Failed exec command. Got output %v:", out.String())
		return trace.Wrap(err)
	}
	//commandOutput := "cstor-pool 1.7.0"
	commandOutput := out.String()
	fmt.Printf("Got pool commandOutput %v:", commandOutput)
	if len(commandOutput) == 0 {
		return trace.Wrap(errors.New("failed to get pool info"))
	}
	poolsAndVersion := strings.Split(commandOutput, "\n")
	fmt.Printf("Got poolsAndVersion %v:", poolsAndVersion)
	for _, poolAndVer := range poolsAndVersion {
		//vav := strings.Split(volAndVer," ")
		upgradeVolume := update.Phase{
			ID:          "openebs-upgrade-pool",
			Description: fmt.Sprintf("Upgrade OpenEBS cStor pool: %v", poolAndVer),
			Executor:    updateOpenEBSPool,
			Data:        &storage.OperationPhaseData{Data: poolAndVer},
		}
		root.AddSequential(upgradeVolume)
	}

	out.Reset()
	// Upgrade volumes

	//	if err := utils.Exec(exec.Command("kubectl", "get", "pv", "-A", "|", "grep", "openebs-cstor","|","cut","-d' '","-f1","|","grep","pvc"), &out); err != nil {
	//	if err := utils.Exec(exec.Command("/bin/bash", "-c", "ls -lath | grep 'drw'  | cut -d' ' -f1 | grep 'drw'"), &out); err != nil {
	//	if err := utils.Exec(exec.Command("/bin/bash", "-c", "ls -lath | grep 'drw'  | cut -d' ' -f1 | grep 'drw'"), &out); err != nil {
	// TODO use kubectl.Command("get","pods","--field-selector","status.phase=Running","--selector=app","cstor-volAndVer-manager,openebs\.io/storage-class=openebs-cstor","-n","openebs","-o","jsonpath='{.items[*].metadata.labels.openebs\.io/persistent-volAndVer}{" "}{.items[*].metadata.labels.openebs\.io/version}'")
	if err := utils.Exec(exec.Command("/bin/bash", "-c", "kubectl get pods --field-selector=status.phase=Running  --selector=app=cstor-volume-manager,openebs\\.io/storage-class=openebs-cstor  -nopenebs -o  jsonpath='{.items[*].metadata.labels.openebs\\.io/persistent-volume}{\" \"}{.items[*].metadata.labels.openebs\\.io/version}'"), &out); err != nil {
		//	p.Warnf("Failed exec command. Got output %v:", out.String())
		return trace.Wrap(err)
	}

	fmt.Printf("Got volumesAndVersion %v:", out.String())
	//commandOutput = "pvc-b363b688-8697-4628-b744-6d943e0b8ed1 1.7.0 pvc-b363b688-8697-4628-b744-6d943e0b8ZZZ 1.7.0"
	commandOutput = out.String()

	fmt.Printf("Got commandOutput for volume: '%v'", commandOutput)
	if len(commandOutput) == 0 {
		return trace.Wrap(errors.New("failed to get pool info"))
	}

	volumesAndVersion := strings.Split(commandOutput, "\n")
	for _, volAndVer := range volumesAndVersion {
		//vav := strings.Split(volAndVer," ")
		// TODO check if the value was extracted correctly
		upgradeVolume := update.Phase{
			ID:          root.ChildLiteral("openebs-upgrade-volume"),
			Description: fmt.Sprintf("Upgrade OpenEBS cStor volume: %v", volAndVer),
			Executor:    updateOpenEBSVolume,
			Data:        &storage.OperationPhaseData{Data: volAndVer},
		}
		root.AddSequential(upgradeVolume)
	}

	return nil
}

func (r phaseBuilder) runtime(updates []loc.Locator) *update.Phase {
	root := update.RootPhase(update.Phase{
		ID:          "runtime",
		Description: "Update application runtime",
	})
	sort.Slice(updates, func(i, j int) bool {
		// Push RBAC package update to front
		return updates[i].Name == constants.BootstrapConfigPackage
	})
	for i, loc := range updates {
		phase := update.Phase{
			ID:       loc.Name,
			Executor: updateApp,
			Description: fmt.Sprintf(
				"Update system application %q to %v", loc.Name, loc.Version),
			Data: &storage.OperationPhaseData{
				Package: &updates[i],
			},
		}
		phase.ID = root.Child(phase)
		root.AddSequential(phase)
	}
	return &root
}

// masters returns a new phase for upgrading master servers.
// leadMaster is the master node that is upgraded first and gets to be the leader during the operation.
// otherMasters lists the rest of the master nodes (can be empty)
func (r phaseBuilder) masters(leadMaster storage.UpdateServer, otherMasters []storage.UpdateServer,
	supportsTaints bool) *update.Phase {
	root := update.RootPhase(update.Phase{
		ID:          "masters",
		Description: "Update master nodes",
	})

	node := r.node(leadMaster.Server, &root, "Update system software on master node %q")
	if len(otherMasters) != 0 {
		node.AddSequential(update.Phase{
			ID:          "kubelet-permissions",
			Executor:    kubeletPermissions,
			Description: fmt.Sprintf("Add permissions to kubelet on %q", leadMaster.Hostname),
			Data: &storage.OperationPhaseData{
				Server: &leadMaster.Server,
			}})

		// election - stepdown first node we will upgrade
		node.AddSequential(setLeaderElection(
			electionChanges{
				id:          "stepdown",
				description: fmt.Sprintf("Step down %q as Kubernetes leader", leadMaster.Hostname),
				disable:     serversToStorage(leadMaster),
			},
			leadMaster,
		))

		// election - force failover to first upgraded master
		electionChanges := electionChanges{
			description: fmt.Sprintf("Make node %q Kubernetes leader", leadMaster.Hostname),
			enable:      serversToStorage(leadMaster),
			disable:     serversToStorage(otherMasters...),
		}

		node.AddSequential(r.commonNode(leadMaster, leadMaster, supportsTaints,
			waitsForEndpoints(false), electionChanges)...)
	} else {
		node.AddSequential(r.commonNode(leadMaster, leadMaster, supportsTaints,
			waitsForEndpoints(true), electionChanges{})...)
	}

	root.AddSequential(node)

	for i, server := range otherMasters {
		node = r.node(server.Server, &root, "Update system software on master node %q")

		electionChanges := electionChanges{
			description: fmt.Sprintf("Enable leader election on node %q", server.Hostname),
			enable:      serversToStorage(server),
		}
		node.AddSequential(r.commonNode(otherMasters[i], leadMaster, supportsTaints,
			waitsForEndpoints(true), electionChanges)...)
		root.AddSequential(node)
	}
	return &root
}

func (r phaseBuilder) nodes(leadMaster storage.UpdateServer, nodes []storage.UpdateServer, supportsTaints bool) *update.Phase {
	root := update.RootPhase(update.Phase{
		ID:          "nodes",
		Description: "Update regular nodes",
	})

	for i, server := range nodes {
		node := r.node(server.Server, &root, "Update system software on node %q")
		node.AddSequential(r.commonNode(nodes[i], leadMaster, supportsTaints,
			waitsForEndpoints(true), electionChanges{})...)
		root.AddParallel(node)
	}
	return &root
}

func (r phaseBuilder) etcdPlan(
	leadMaster storage.Server,
	otherMasters []storage.Server,
	workers []storage.Server,
	currentVersion string,
	desiredVersion string,
) *update.Phase {
	root := update.RootPhase(update.Phase{
		ID:          etcdPhaseName,
		Description: fmt.Sprintf("Upgrade etcd %v to %v", currentVersion, desiredVersion),
	})
	if currentVersion == "" {
		root.Description = fmt.Sprintf("Upgrade etcd to %v", desiredVersion)
	}

	// Backup etcd on each master server
	// Do each master, just in case
	backupEtcd := update.Phase{
		ID:          root.ChildLiteral("backup"),
		Description: "Backup etcd data",
	}
	backupEtcd.AddParallel(r.etcdBackupNode(leadMaster, backupEtcd))

	for _, server := range otherMasters {
		p := r.etcdBackupNode(server, backupEtcd)
		backupEtcd.AddParallel(p)
	}

	root.AddSequential(backupEtcd)

	// Shutdown etcd
	// Move data directory to backup location
	shutdownEtcd := update.Phase{
		ID:          root.ChildLiteral("shutdown"),
		Description: "Shutdown etcd cluster",
	}
	shutdownEtcd.AddWithDependency(
		update.DependencyForServer(backupEtcd, leadMaster),
		r.etcdShutdownNode(leadMaster, shutdownEtcd, true))

	for _, server := range otherMasters {
		p := r.etcdShutdownNode(server, shutdownEtcd, false)
		shutdownEtcd.AddWithDependency(update.DependencyForServer(backupEtcd, server), p)
	}

	root.Add(shutdownEtcd)

	// Upgrade servers
	// Replace configuration and data directories, for new version of etcd
	// relaunch etcd on temporary port
	upgradeServers := update.Phase{
		ID:          root.ChildLiteral("upgrade"),
		Description: "Upgrade etcd servers",
	}
	upgradeServers.AddWithDependency(
		update.DependencyForServer(shutdownEtcd, leadMaster),
		r.etcdUpgrade(leadMaster, upgradeServers))

	for _, server := range otherMasters {
		p := r.etcdUpgrade(server, upgradeServers)
		upgradeServers.AddWithDependency(update.DependencyForServer(shutdownEtcd, server), p)
	}
	root.Add(upgradeServers)

	// Restore kubernetes data
	// migrate to etcd3 store
	// clear kubernetes data from etcd2 store
	restoreData := update.Phase{
		ID:          root.ChildLiteral("restore"),
		Description: "Restore etcd data from backup",
		Executor:    updateEtcdRestore,
		Data: &storage.OperationPhaseData{
			Server: &leadMaster,
		},
	}
	root.AddSequential(restoreData)

	// restart master servers
	// Rolling restart of master servers to listen on normal ports. ETCD outage ends here
	restartMasters := update.Phase{
		ID:          root.ChildLiteral("restart"),
		Description: "Restart etcd servers",
	}
	restartMasters.AddWithDependency(
		update.DependencyForServer(restoreData, leadMaster),
		r.etcdRestart(leadMaster, leadMaster, restartMasters))

	for _, server := range otherMasters {
		p := r.etcdRestart(server, leadMaster, restartMasters)
		restartMasters.AddWithDependency(update.DependencyForServer(upgradeServers, server), p)
	}

	// also restart gravity-site, so that elections get unbroken
	restartMasters.AddParallel(update.Phase{
		ID:          restartMasters.ChildLiteral(constants.GravityServiceName),
		Description: fmt.Sprint("Restart ", constants.GravityServiceName, " service"),
		Executor:    updateEtcdRestartGravity,
		Data: &storage.OperationPhaseData{
			Server: &leadMaster,
		},
	})
	root.Add(restartMasters)

	return &root
}

func (r phaseBuilder) etcdBackupNode(server storage.Server, parent update.Phase) update.Phase {
	return update.Phase{
		ID:          parent.ChildLiteral(server.Hostname),
		Description: fmt.Sprintf("Backup etcd on node %q", server.Hostname),
		Executor:    updateEtcdBackup,
		Data: &storage.OperationPhaseData{
			Server: &server,
		},
	}
}

func (r phaseBuilder) etcdShutdownNode(server storage.Server, parent update.Phase, isLeader bool) update.Phase {
	return update.Phase{
		ID:          parent.ChildLiteral(server.Hostname),
		Description: fmt.Sprintf("Shutdown etcd on node %q", server.Hostname),
		Executor:    updateEtcdShutdown,
		Data: &storage.OperationPhaseData{
			Server: &server,
			Data:   strconv.FormatBool(isLeader),
		},
	}
}

func (r phaseBuilder) etcdUpgrade(server storage.Server, parent update.Phase) update.Phase {
	return update.Phase{
		ID:          parent.ChildLiteral(server.Hostname),
		Description: fmt.Sprintf("Upgrade etcd on node %q", server.Hostname),
		Executor:    updateEtcdMaster,
		Data: &storage.OperationPhaseData{
			Server: &server,
		},
	}
}

func (r phaseBuilder) etcdRestart(server storage.Server, leadMaster storage.Server, parent update.Phase) update.Phase {
	return update.Phase{
		ID:          parent.ChildLiteral(server.Hostname),
		Description: fmt.Sprintf("Restart etcd on node %q", server.Hostname),
		Executor:    updateEtcdRestart,
		Data: &storage.OperationPhaseData{
			Server: &server,
			Master: &leadMaster,
		},
	}
}

func (r phaseBuilder) node(server storage.Server, parent update.ParentPhase, format string) update.Phase {
	return update.Phase{
		ID:          parent.ChildLiteral(server.Hostname),
		Description: fmt.Sprintf(format, server.Hostname),
	}
}

// commonNode returns a list of operations required for any node role to upgrade its system software
func (r phaseBuilder) commonNode(server, leadMaster storage.UpdateServer, supportsTaints bool,
	waitsForEndpoints waitsForEndpoints, electionChanges electionChanges) []update.Phase {
	phases := []update.Phase{
		{
			ID:          "drain",
			Executor:    drainNode,
			Description: fmt.Sprintf("Drain node %q", server.Hostname),
			Data: &storage.OperationPhaseData{
				Server:     &server.Server,
				ExecServer: &leadMaster.Server,
			},
		},
		{
			ID:          "system-upgrade",
			Executor:    updateSystem,
			Description: fmt.Sprintf("Update system software on node %q", server.Hostname),
			Data: &storage.OperationPhaseData{
				ExecServer: &server.Server,
				Update: &storage.UpdateOperationData{
					Servers: []storage.UpdateServer{server},
				},
			},
		},
	}
	if electionChanges.shouldAddPhase() {
		phases = append(phases,
			setLeaderElection(
				electionChanges,
				server,
			),
		)
	}
	phases = append(phases, update.Phase{
		ID:          "health",
		Executor:    nodeHealth,
		Description: fmt.Sprintf("Health check node %q", server.Hostname),
		Data: &storage.OperationPhaseData{
			Server: &server.Server,
		},
	})
	if supportsTaints {
		phases = append(phases, update.Phase{
			ID:          "taint",
			Executor:    taintNode,
			Description: fmt.Sprintf("Taint node %q", server.Hostname),
			Data: &storage.OperationPhaseData{
				Server:     &server.Server,
				ExecServer: &leadMaster.Server,
			}})
	}
	phases = append(phases, update.Phase{
		ID:          "uncordon",
		Executor:    uncordonNode,
		Description: fmt.Sprintf("Uncordon node %q", server.Hostname),
		Data: &storage.OperationPhaseData{
			Server:     &server.Server,
			ExecServer: &leadMaster.Server,
		},
	})
	if waitsForEndpoints {
		phases = append(phases, update.Phase{
			ID:          "endpoints",
			Executor:    endpoints,
			Description: fmt.Sprintf("Wait for DNS/cluster endpoints on %q", server.Hostname),
			Data: &storage.OperationPhaseData{
				Server:     &server.Server,
				ExecServer: &leadMaster.Server,
			},
		})
	}
	if supportsTaints {
		phases = append(phases, update.Phase{
			ID:          "untaint",
			Executor:    untaintNode,
			Description: fmt.Sprintf("Remove taint from node %q", server.Hostname),
			Data: &storage.OperationPhaseData{
				Server:     &server.Server,
				ExecServer: &leadMaster.Server,
			},
		})
	}
	return phases
}

func (r phaseBuilder) cleanup() *update.Phase {
	root := update.RootPhase(update.Phase{
		ID:          "gc",
		Description: "Run cleanup tasks",
	})

	for i := range r.servers {
		node := r.node(r.servers[i].Server, &root, "Clean up node %q")
		node.Executor = cleanupNode
		node.Data = &storage.OperationPhaseData{
			Server: &r.servers[i].Server,
		}
		root.AddParallel(node)
	}
	return &root
}

type phaseBuilder struct {
	planConfig
}

func shouldUpdateCoreDNS(client *kubernetes.Clientset) (bool, error) {
	_, err := client.RbacV1().ClusterRoles().Get(libphase.CoreDNSResourceName, metav1.GetOptions{})
	err = rigging.ConvertError(err)
	if err != nil {
		if trace.IsNotFound(err) {
			return true, nil
		}
		return false, trace.Wrap(err)
	}

	_, err = client.RbacV1().ClusterRoleBindings().Get(libphase.CoreDNSResourceName, metav1.GetOptions{})
	err = rigging.ConvertError(err)
	if err != nil {
		if trace.IsNotFound(err) {
			return true, nil
		}
		return false, trace.Wrap(err)
	}

	_, err = client.CoreV1().ConfigMaps(constants.KubeSystemNamespace).Get("coredns", metav1.GetOptions{})
	err = rigging.ConvertError(err)
	if err != nil {
		if trace.IsNotFound(err) {
			return true, nil
		}
		return false, trace.Wrap(err)
	}

	return false, nil
}

// supportsTaints determines if the specified gravity package
// supports node taints.
func supportsTaints(gravityPackage loc.Locator) (supports bool, err error) {
	ver, err := gravityPackage.SemVer()
	if err != nil {
		return false, trace.Wrap(err)
	}
	return defaults.BaseTaintsVersion.Compare(*ver) <= 0, nil
}

func shouldUpdateEtcd(p planConfig) (updateEtcd bool, installedEtcdVersion string, updateEtcdVersion string, err error) {
	// TODO: should somehow maintain etcd version invariant across runtime packages
	runtimePackage, err := p.installedRuntime.Manifest.DefaultRuntimePackage()
	if err != nil && !trace.IsNotFound(err) {
		return false, "", "", trace.Wrap(err)
	}
	if err != nil {
		runtimePackage, err = p.installedRuntime.Manifest.Dependencies.ByName(loc.LegacyPlanetMaster.Name)
		if err != nil {
			log.Warnf("Failed to fetch the runtime package: %v.", err)
			return false, "", "", trace.NotFound("runtime package not found")
		}
	}
	installedVersion, err := getEtcdVersion("version-etcd", *runtimePackage, p.packageService)
	if err != nil {
		if !trace.IsNotFound(err) {
			return false, "", "", trace.Wrap(err)
		}
		// if the currently installed version doesn't have etcd version information, it needs to be upgraded
		updateEtcd = true
	}
	runtimePackage, err = p.updateRuntime.Manifest.DefaultRuntimePackage()
	if err != nil {
		return false, "", "", trace.Wrap(err)
	}
	updateVersion, err := getEtcdVersion("version-etcd", *runtimePackage, p.packageService)
	if err != nil {
		return false, "", "", trace.Wrap(err)
	}
	if installedVersion == nil || installedVersion.Compare(*updateVersion) < 0 {
		updateEtcd = true
	}
	if installedVersion != nil {
		installedEtcdVersion = installedVersion.String()
	}
	updateEtcdVersion = updateVersion.String()
	return updateEtcd, installedEtcdVersion, updateEtcdVersion, nil
}

func getEtcdVersion(searchLabel string, locator loc.Locator, packageService pack.PackageService) (*semver.Version, error) {
	manifest, err := pack.GetPackageManifest(packageService, locator)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	for _, label := range manifest.Labels {
		if label.Name == searchLabel {
			versionS := strings.TrimPrefix(label.Value, "v")
			version, err := semver.NewVersion(versionS)
			if err != nil {
				return nil, trace.Wrap(err)
			}
			return version, nil
		}
	}
	return nil, trace.NotFound("package manifest for %q does not have label %v",
		locator, searchLabel)
}

// setLeaderElection creates a phase that will change the leader election state in the cluster
// enable - the list of servers to enable election on
// disable - the list of servers to disable election on
// server - The server the phase should be executed on, and used to name the phase
// key - is the identifier of the phase (combined with server.Hostname)
// msg - is a format string used to describe the phase
func setLeaderElection(electionChanges electionChanges, server storage.UpdateServer) update.Phase {
	return update.Phase{
		ID:          electionChanges.ID(),
		Executor:    electionStatus,
		Description: electionChanges.description,
		Data: &storage.OperationPhaseData{
			Server: &server.Server,
			ElectionChange: &storage.ElectionChange{
				EnableServers:  electionChanges.enable,
				DisableServers: electionChanges.disable,
			},
		},
	}
}

func serversToStorage(updates ...storage.UpdateServer) (result []storage.Server) {
	for _, update := range updates {
		result = append(result, update.Server)
	}
	return result
}

type electionChanges struct {
	enable      []storage.Server
	disable     []storage.Server
	description string
	id          string
}

func (e electionChanges) shouldAddPhase() bool {
	if len(e.enable) != 0 || len(e.disable) != 0 {
		return true
	}
	return false
}

func (e electionChanges) ID() string {
	if e.id != "" {
		return e.id
	}
	return "elect"
}

type waitsForEndpoints bool
type enableElections bool

const etcdPhaseName = "etcd"
