// Copyright 2016 flannel authors
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

package kube

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"net"
	"os"
	"time"

	"github.com/coreos/flannel/pkg/ip"
	"github.com/coreos/flannel/subnet"

	"github.com/golang/glog"
	"golang.org/x/net/context"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/strategicpatch"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apimachinery/pkg/watch"
	clientset "k8s.io/client-go/kubernetes"
	listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/pkg/api"
	"k8s.io/client-go/pkg/api/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
)

var (
	ErrUnimplemented = errors.New("unimplemented")
)

const (
	resyncPeriod              = 5 * time.Minute
	nodeControllerSyncTimeout = 10 * time.Minute
)

// k8s的子网管理器
type kubeSubnetManager struct {
	annotations    annotations
	client         clientset.Interface
	nodeName       string
	nodeStore      listers.NodeLister
	nodeController cache.Controller
	subnetConf     *subnet.Config
	// 一个分配子网的消息或者删除一个子网的消息
	events chan subnet.Event
}

func NewSubnetManager(apiUrl, kubeconfig, prefix, netConfPath string) (subnet.Manager, error) {

	var cfg *rest.Config
	var err error
	// Try to build kubernetes config from a master url or a kubeconfig filepath. If neither masterUrl
	// or kubeconfigPath are passed in we fall back to inClusterConfig. If inClusterConfig fails,
	// we fallback to the default config.
	cfg, err = clientcmd.BuildConfigFromFlags(apiUrl, kubeconfig)
	if err != nil {
		return nil, fmt.Errorf("fail to create kubernetes config: %v", err)
	}

	c, err := clientset.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("unable to initialize client: %v", err)
	}

	// The kube subnet mgr needs to know the k8s node name that it's running on so it can annotate it.
	// If we're running as a pod then the POD_NAME and POD_NAMESPACE will be populated and can be used to find the node
	// name. Otherwise, the environment variable NODE_NAME can be passed in.
	nodeName := os.Getenv("NODE_NAME")
	if nodeName == "" {
		podName := os.Getenv("POD_NAME")
		podNamespace := os.Getenv("POD_NAMESPACE")
		if podName == "" || podNamespace == "" {
			return nil, fmt.Errorf("env variables POD_NAME and POD_NAMESPACE must be set")
		}

		pod, err := c.Pods(podNamespace).Get(podName, metav1.GetOptions{})
		if err != nil {
			return nil, fmt.Errorf("error retrieving pod spec for '%s/%s': %v", podNamespace, podName, err)
		}
		nodeName = pod.Spec.NodeName
		if nodeName == "" {
			return nil, fmt.Errorf("node name not present in pod spec '%s/%s'", podNamespace, podName)
		}
	}

	netConf, err := ioutil.ReadFile(netConfPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read net conf: %v", err)
	}

	sc, err := subnet.ParseConfig(string(netConf))
	if err != nil {
		return nil, fmt.Errorf("error parsing subnet config: %s", err)
	}
	// 为节点创建子网管理器
	sm, err := newKubeSubnetManager(c, sc, nodeName, prefix)
	if err != nil {
		return nil, fmt.Errorf("error creating network manager: %s", err)
	}
	// 运行子网管理器
	go sm.Run(context.Background())

	// 等待controller完成初始化
	glog.Infof("Waiting %s for node controller to sync", nodeControllerSyncTimeout)
	err = wait.Poll(time.Second, nodeControllerSyncTimeout, func() (bool, error) {
		return sm.nodeController.HasSynced(), nil
	})
	if err != nil {
		return nil, fmt.Errorf("error waiting for nodeController to sync state: %v", err)
	}
	glog.Infof("Node controller sync successful")

	return sm, nil
}

func newKubeSubnetManager(c clientset.Interface, sc *subnet.Config, nodeName, prefix string) (*kubeSubnetManager, error) {
	var err error
	var ksm kubeSubnetManager
	ksm.annotations, err = newAnnotations(prefix)
	if err != nil {
		return nil, err
	}
	ksm.client = c
	ksm.nodeName = nodeName
	ksm.subnetConf = sc
	ksm.events = make(chan subnet.Event, 5000)
	indexer, controller := cache.NewIndexerInformer(
		// 对node感兴趣，监听node状态，及时为node添加子网
		&cache.ListWatch{
			ListFunc: func(options metav1.ListOptions) (runtime.Object, error) {
				return ksm.client.CoreV1().Nodes().List(options)
			},
			WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
				return ksm.client.CoreV1().Nodes().Watch(options)
			},
		},
		&v1.Node{},
		resyncPeriod,
		// 从deltafifo队列中取出来调用该方法
		cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				ksm.handleAddLeaseEvent(subnet.EventAdded, obj)
			},
			UpdateFunc: ksm.handleUpdateLeaseEvent,
			DeleteFunc: func(obj interface{}) {
				node, isNode := obj.(*v1.Node)
				// We can get DeletedFinalStateUnknown instead of *api.Node here and we need to handle that correctly.
				if !isNode {
					deletedState, ok := obj.(cache.DeletedFinalStateUnknown)
					if !ok {
						glog.Infof("Error received unexpected object: %v", obj)
						return
					}
					node, ok = deletedState.Obj.(*v1.Node)
					if !ok {
						glog.Infof("Error deletedFinalStateUnknown contained non-Node object: %v", deletedState.Obj)
						return
					}
					obj = node
				}
				ksm.handleAddLeaseEvent(subnet.EventRemoved, obj)
			},
		},
		cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc},
	)
	ksm.nodeController = controller
	ksm.nodeStore = listers.NewNodeLister(indexer)
	return &ksm, nil
}

func (ksm *kubeSubnetManager) handleAddLeaseEvent(et subnet.EventType, obj interface{}) {
	n := obj.(*v1.Node)
	if s, ok := n.Annotations[ksm.annotations.SubnetKubeManaged]; !ok || s != "true" {
		return
	}

	l, err := ksm.nodeToLease(*n)
	if err != nil {
		glog.Infof("Error turning node %q to lease: %v", n.ObjectMeta.Name, err)
		return
	}
	ksm.events <- subnet.Event{et, l}
}

func (ksm *kubeSubnetManager) handleUpdateLeaseEvent(oldObj, newObj interface{}) {
	o := oldObj.(*v1.Node)
	n := newObj.(*v1.Node)
	if s, ok := n.Annotations[ksm.annotations.SubnetKubeManaged]; !ok || s != "true" {
		return
	}
	if o.Annotations[ksm.annotations.BackendData] == n.Annotations[ksm.annotations.BackendData] &&
		o.Annotations[ksm.annotations.BackendType] == n.Annotations[ksm.annotations.BackendType] &&
		o.Annotations[ksm.annotations.BackendPublicIP] == n.Annotations[ksm.annotations.BackendPublicIP] {
		return // No change to lease
	}

	l, err := ksm.nodeToLease(*n)
	if err != nil {
		glog.Infof("Error turning node %q to lease: %v", n.ObjectMeta.Name, err)
		return
	}
	ksm.events <- subnet.Event{subnet.EventAdded, l}
}

func (ksm *kubeSubnetManager) GetNetworkConfig(ctx context.Context) (*subnet.Config, error) {
	return ksm.subnetConf, nil
}

func (ksm *kubeSubnetManager) AcquireLease(ctx context.Context, attrs *subnet.LeaseAttrs) (*subnet.Lease, error) {
	cachedNode, err := ksm.nodeStore.Get(ksm.nodeName)
	if err != nil {
		return nil, err
	}
	nobj, err := api.Scheme.DeepCopy(cachedNode)
	if err != nil {
		return nil, err
	}
	n := nobj.(*v1.Node)

	if n.Spec.PodCIDR == "" {
		return nil, fmt.Errorf("node %q pod cidr not assigned", ksm.nodeName)
	}
	bd, err := attrs.BackendData.MarshalJSON()
	if err != nil {
		return nil, err
	}
	_, cidr, err := net.ParseCIDR(n.Spec.PodCIDR)
	if err != nil {
		return nil, err
	}
	if n.Annotations[ksm.annotations.BackendData] != string(bd) ||
		n.Annotations[ksm.annotations.BackendType] != attrs.BackendType ||
		n.Annotations[ksm.annotations.BackendPublicIP] != attrs.PublicIP.String() ||
		n.Annotations[ksm.annotations.SubnetKubeManaged] != "true" ||
		(n.Annotations[ksm.annotations.BackendPublicIPOverwrite] != "" && n.Annotations[ksm.annotations.BackendPublicIPOverwrite] != attrs.PublicIP.String()) {
		n.Annotations[ksm.annotations.BackendType] = attrs.BackendType
		n.Annotations[ksm.annotations.BackendData] = string(bd)
		if n.Annotations[ksm.annotations.BackendPublicIPOverwrite] != "" {
			if n.Annotations[ksm.annotations.BackendPublicIP] != n.Annotations[ksm.annotations.BackendPublicIPOverwrite] {
				glog.Infof("Overriding public ip with '%s' from node annotation '%s'",
					n.Annotations[ksm.annotations.BackendPublicIPOverwrite],
					ksm.annotations.BackendPublicIPOverwrite)
				n.Annotations[ksm.annotations.BackendPublicIP] = n.Annotations[ksm.annotations.BackendPublicIPOverwrite]
			}
		} else {
			n.Annotations[ksm.annotations.BackendPublicIP] = attrs.PublicIP.String()
		}
		n.Annotations[ksm.annotations.SubnetKubeManaged] = "true"

		oldData, err := json.Marshal(cachedNode)
		if err != nil {
			return nil, err
		}

		newData, err := json.Marshal(n)
		if err != nil {
			return nil, err
		}

		patchBytes, err := strategicpatch.CreateTwoWayMergePatch(oldData, newData, v1.Node{})
		if err != nil {
			return nil, fmt.Errorf("failed to create patch for node %q: %v", ksm.nodeName, err)
		}

		// 这里把新的网络提交给apiserver
		_, err = ksm.client.CoreV1().Nodes().Patch(ksm.nodeName, types.StrategicMergePatchType, patchBytes, "status")
		if err != nil {
			return nil, err
		}
	}
	// 开始调度节点的网络
	err = ksm.setNodeNetworkUnavailableFalse()
	if err != nil {
		glog.Errorf("Unable to set NetworkUnavailable to False for %q: %v", ksm.nodeName, err)
	}
	return &subnet.Lease{
		Subnet:     ip.FromIPNet(cidr),
		Attrs:      *attrs,
		Expiration: time.Now().Add(24 * time.Hour),
	}, nil
}

func (ksm *kubeSubnetManager) WatchLeases(ctx context.Context, cursor interface{}) (subnet.LeaseWatchResult, error) {
	select {
	case event := <-ksm.events:
		return subnet.LeaseWatchResult{
			Events: []subnet.Event{event},
		}, nil
	case <-ctx.Done():
		return subnet.LeaseWatchResult{}, nil
	}
}

func (ksm *kubeSubnetManager) Run(ctx context.Context) {
	glog.Infof("Starting kube subnet manager")
	ksm.nodeController.Run(ctx.Done())
}

// 从node中获取到子网相关的信息
func (ksm *kubeSubnetManager) nodeToLease(n v1.Node) (l subnet.Lease, err error) {
	l.Attrs.PublicIP, err = ip.ParseIP4(n.Annotations[ksm.annotations.BackendPublicIP])
	if err != nil {
		return l, err
	}

	l.Attrs.BackendType = n.Annotations[ksm.annotations.BackendType]
	l.Attrs.BackendData = json.RawMessage(n.Annotations[ksm.annotations.BackendData])

	_, cidr, err := net.ParseCIDR(n.Spec.PodCIDR)
	if err != nil {
		return l, err
	}

	l.Subnet = ip.FromIPNet(cidr)
	return l, nil
}

// RenewLease: unimplemented
func (ksm *kubeSubnetManager) RenewLease(ctx context.Context, lease *subnet.Lease) error {
	return ErrUnimplemented
}

func (ksm *kubeSubnetManager) WatchLease(ctx context.Context, sn ip.IP4Net, cursor interface{}) (subnet.LeaseWatchResult, error) {
	return subnet.LeaseWatchResult{}, ErrUnimplemented
}

func (ksm *kubeSubnetManager) Name() string {
	return fmt.Sprintf("Kubernetes Subnet Manager - %s", ksm.nodeName)
}

// Set Kubernetes NodeNetworkUnavailable to false when starting
// https://kubernetes.io/docs/concepts/architecture/nodes/#condition
func (ksm *kubeSubnetManager) setNodeNetworkUnavailableFalse() error {
	condition := v1.NodeCondition{
		Type:               v1.NodeNetworkUnavailable,
		Status:             v1.ConditionFalse,
		Reason:             "FlannelIsUp",
		Message:            "Flannel is running on this node",
		LastTransitionTime: metav1.Now(),
		LastHeartbeatTime:  metav1.Now(),
	}
	raw, err := json.Marshal(&[]v1.NodeCondition{condition})
	if err != nil {
		return err
	}
	patch := []byte(fmt.Sprintf(`{"status":{"conditions":%s}}`, raw))
	_, err = ksm.client.CoreV1().Nodes().PatchStatus(ksm.nodeName, patch)
	return err
}
