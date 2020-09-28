/*
Copyright 2020 Red Hat

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

package controllers

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	ovncentralv1alpha1 "github.com/openstack-k8s-operators/ovn-central-operator/api/v1alpha1"
	"github.com/openstack-k8s-operators/ovn-central-operator/util"
)

// OVSDBClusterReconciler reconciles a OVSDBCluster object
type OVSDBClusterReconciler struct {
	client.Client
	Log    logr.Logger
	Scheme *runtime.Scheme
}

// ReconcilerCommon

func (r *OVSDBClusterReconciler) GetClient() client.Client {
	return r.Client
}

func (r *OVSDBClusterReconciler) GetLogger() logr.Logger {
	return r.Log
}

const (
	OVSDBClusterLabel = "ovsdb-cluster"
	OVSDBServerLabel  = "ovsdb-server"
)

// +kubebuilder:rbac:groups=ovn-central.openstack.org,resources=ovsdbclusters,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=ovn-central.openstack.org,resources=ovsdbclusters/status,verbs=get;update;patch

func (r *OVSDBClusterReconciler) Reconcile(req ctrl.Request) (result ctrl.Result, err error) {
	ctx := context.Background()
	_ = r.Log.WithValues("ovsdbcluster", req.NamespacedName)

	//
	// Fetch the cluster object
	//

	cluster := &ovncentralv1alpha1.OVSDBCluster{}
	if err := r.Client.Get(ctx, req.NamespacedName, cluster); err != nil {
		if errors.IsNotFound(err) {
			// Request object not found, could have been deleted after
			// reconcile request. Owned objects are automatically garbage
			// collected. For additional cleanup logic use finalizers.
			// Return and don't requeue.
			return ctrl.Result{}, nil
		}
		err = WrapErrorForObject("Get cluster", cluster, err)
		return ctrl.Result{}, err
	}

	//
	// Snapshot the original status object and ensure we save any changes to it on return
	//

	origStatus := cluster.Status.DeepCopy()
	statusChanged := func() bool {
		return !equality.Semantic.DeepEqual(&cluster.Status, origStatus)
	}

	defer func() {
		if statusChanged() {
			if updateErr := r.Client.Status().Update(ctx, cluster); updateErr != nil {
				if err == nil {
					err = WrapErrorForObject(
						"Update Status", cluster, updateErr)
				} else {
					LogErrorForObject(r, updateErr, "Update status", cluster)
				}
			}
		}
	}()

	// Unset the Failed condition. This ensures that the Failed condition will be unset
	// automatically if anything in the cluster is changing, and will only persist if the
	// Failure condition persists.
	util.UnsetFailed(cluster)

	//
	// Get all the OVSDBServers we manage
	//

	servers, err := r.getServers(ctx, cluster)
	if err != nil {
		return ctrl.Result{}, err
	}

	findServer := func(name string) *ovncentralv1alpha1.OVSDBServer {
		for i := 0; i < len(servers); i++ {
			if servers[i].Name == name {
				return &servers[i]
			}
		}
		return nil
	}

	//
	// Get all the DB server Pods we manage
	//

	serverPods, err := r.getServerPods(ctx, cluster)
	if err != nil {
		return ctrl.Result{}, err
	}

	findPod := func(name string) *corev1.Pod {
		for i := 0; i < len(serverPods); i++ {
			if serverPods[i].Name == name {
				return &serverPods[i]
			}
		}
		return nil
	}

	//
	// We're Available iff a quorum of server pods are Ready
	//
	// Quorum is based on the number of servers which have been initialised into the cluster,
	// not the target number of replicas.
	//

	clusterSize := 0
	for i := 0; i < len(servers); i++ {
		if util.IsAvailable(&servers[i]) {
			clusterSize++
		}
	}
	clusterQuorum := int(math.Ceil(float64(clusterSize) / 2))

	nAvailable := 0
	for i := 0; i < len(serverPods); i++ {
		if util.IsPodReady(&serverPods[i]) {
			nAvailable++
		}
	}

	if nAvailable >= clusterQuorum && nAvailable > 0 {
		util.SetAvailable(cluster)
	} else {
		util.UnsetAvailable(cluster)
	}

	cluster.Status.AvailableServers = nAvailable
	cluster.Status.ClusterSize = clusterSize
	cluster.Status.ClusterQuorum = clusterQuorum

	//
	// If any servers have failed, also set failed on the cluster
	//

	for i := 0; i < len(servers); i++ {
		var failed []string
		if util.IsFailed(&servers[i]) {
			failed = append(failed, servers[i].Name)
		}

		if len(failed) > 0 {
			msg := fmt.Sprintf("The following servers have failed to intialize: %s",
				strings.Join(failed, ", "))
			util.SetFailed(cluster, ovncentralv1alpha1.OVSDBClusterBootstrap, msg)
		}
	}

	//
	// Set ClusterID from server ClusterIDs
	//

	for _, server := range servers {
		if cluster.Status.ClusterID == nil {
			cluster.Status.ClusterID = server.Status.ClusterID
		} else {
			if server.Status.ClusterID != nil &&
				*cluster.Status.ClusterID != *server.Status.ClusterID {

				// N.B. This overwrites a ClusterBootstrap failure. I guess this is
				// ok, as we can only set 1 failure condition and it's arbitrary
				// which one.
				msg := fmt.Sprintf("Server %s has inconsistent ClusterID %s. "+
					"Expected ClusterID %s",
					server.Name, *server.Status.ClusterID,
					*cluster.Status.ClusterID)
				util.SetFailed(
					cluster,
					ovncentralv1alpha1.OVSDBClusterInconsistent, msg)
			}

		}
	}

	// Status will be saved automatically
	if statusChanged() {
		return ctrl.Result{}, nil
	}

	//
	// Scale up servers if required
	//

	targetServers := cluster.Spec.Replicas
	if targetServers == 0 {
		// The cluster needs at least one server as a datastore, even if it's not running
		targetServers = 1
	}
	if cluster.Status.ClusterID == nil {
		// Create exactly one server if we're not bootstrapped
		targetServers = 1
	}

	for i := len(servers); i < targetServers; i++ {
		name := nextServerName(cluster, findServer)
		server := serverShell(cluster, name)
		apply := func() error {
			serverApply(cluster, server, servers)

			err := controllerutil.SetControllerReference(
				cluster, server, r.Scheme)
			if err != nil {
				return WrapErrorForObject(
					"Set controller reference for server", server, err)
			}

			return err
		}

		NeedsUpdate(r, ctx, server, apply)
		_, err := controllerutil.CreateOrUpdate(ctx, r.Client, server, apply)
		if err != nil {
			return ctrl.Result{}, err
		}

		LogForObject(r, "Created server", server)
	}

	//
	// Ensure we have a pod for each available server
	//

	if cluster.Spec.Replicas == 0 {
		// If we scale down to zero we'll still have 1 server, but we don't want any
		// running pods.
		for i := 0; i < len(serverPods); i++ {
			serverPod := &serverPods[i]
			if err := r.Delete(ctx, serverPod); err != nil {
				err = WrapErrorForObject("Delete server pod", &serverPods[i], err)
				return ctrl.Result{}, err
			}
			LogForObject(r, "Deleted server pod", serverPod)
		}
	} else {
		for i := 0; i < len(servers); i++ {
			server := &servers[i]
			if !util.IsAvailable(server) {
				// Wait for the server to bootstrap
				continue
			}

			serverPod := findPod(server.Name)

			// If the pod already exists, updating could potentially cause an outage of
			// it.
			if serverPod != nil {
				// Updating a cluster with less than 3 servers will always cause
				// loss of quorum, so just do it.
				if clusterSize >= 3 && nAvailable <= clusterQuorum {
					continue
				}
			} else {
				serverPod = dbServerShell(server)
			}

			apply := func() error {
				dbServerApply(serverPod, server, cluster)

				if err := controllerutil.SetControllerReference(
					cluster, serverPod, r.Scheme); err != nil {

					err = WrapErrorForObject(
						"Set controller reference for server pod",
						serverPod, err)
					return err
				}
				return nil
			}
			op, err := CreateOrDelete(r, ctx, serverPod, apply)
			if err != nil {
				err = WrapErrorForObject("Update server pod", serverPod, err)
				return ctrl.Result{}, err
			}
			if op != controllerutil.OperationResultNone {
				nAvailable -= 1
			}
		}
	}

	// FIN
	return ctrl.Result{}, nil
}

func nextServerName(
	cluster *ovncentralv1alpha1.OVSDBCluster,
	findServer func(name string) *ovncentralv1alpha1.OVSDBServer) string {

	for i := 0; ; i++ {
		name := fmt.Sprintf("%s-%d", cluster.Name, i)
		if findServer(name) == nil {
			return name
		}
	}
}

func (r *OVSDBClusterReconciler) getServers(
	ctx context.Context,
	cluster *ovncentralv1alpha1.OVSDBCluster) ([]ovncentralv1alpha1.OVSDBServer, error) {

	serverList := &ovncentralv1alpha1.OVSDBServerList{}
	serverListOpts := &client.ListOptions{Namespace: cluster.Namespace}
	client.MatchingLabels{
		OVSDBClusterLabel: cluster.Name,
	}.ApplyToList(serverListOpts)
	if err := r.Client.List(ctx, serverList, serverListOpts); err != nil {
		err = fmt.Errorf("Error listing servers for cluster %s: %w", cluster.Name, err)
		return nil, err
	}

	servers := serverList.Items
	sort.Slice(servers, func(i, j int) bool {
		return servers[i].Name < servers[j].Name
	})

	return servers, nil
}

func (r *OVSDBClusterReconciler) getServerPods(
	ctx context.Context,
	cluster *ovncentralv1alpha1.OVSDBCluster) ([]corev1.Pod, error) {

	podList := &corev1.PodList{}
	podListOpts := &client.ListOptions{Namespace: cluster.Namespace}
	client.MatchingLabels{
		"app":             "ovsdb-server",
		OVSDBClusterLabel: cluster.Name,
	}.ApplyToList(podListOpts)
	if err := r.Client.List(ctx, podList, podListOpts); err != nil {
		err = fmt.Errorf("Error listing server pods for cluster %s: %w", cluster.Name, err)
		return nil, err
	}

	pods := podList.Items
	sort.Slice(pods, func(i, j int) bool {
		return pods[i].Name < pods[j].Name
	})

	return pods, nil
}

func (r *OVSDBClusterReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&ovncentralv1alpha1.OVSDBCluster{}).
		Owns(&ovncentralv1alpha1.OVSDBServer{}).
		Owns(&corev1.Pod{}).
		Complete(r)
}

func serverShell(
	cluster *ovncentralv1alpha1.OVSDBCluster,
	name string) *ovncentralv1alpha1.OVSDBServer {

	server := &ovncentralv1alpha1.OVSDBServer{}
	server.Name = name
	server.Namespace = cluster.Namespace
	return server
}

func serverApply(
	cluster *ovncentralv1alpha1.OVSDBCluster,
	server *ovncentralv1alpha1.OVSDBServer,
	allServers []ovncentralv1alpha1.OVSDBServer) {

	var initPeers []string
	for _, peer := range allServers {
		if peer.Name != server.Name && peer.Status.RaftAddress != nil {
			initPeers = append(initPeers, *peer.Status.RaftAddress)
		}
	}

	util.InitLabelMap(&server.Labels)
	server.Labels[OVSDBClusterLabel] = cluster.Name

	server.Spec.DBType = cluster.Spec.DBType
	server.Spec.ClusterID = cluster.Status.ClusterID
	server.Spec.ClusterName = cluster.Name
	server.Spec.InitPeers = initPeers

	server.Spec.StorageSize = cluster.Spec.ServerStorageSize
	server.Spec.StorageClass = cluster.Spec.ServerStorageClass
}