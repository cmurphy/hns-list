package apiresources

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cmurphy/hns-list/pkg/consts"
	apiextcontrollerv1 "github.com/rancher/wrangler/pkg/generated/controllers/apiextensions.k8s.io/v1"
	apiregcontrollerv1 "github.com/rancher/wrangler/pkg/generated/controllers/apiregistration.k8s.io/v1"
	"github.com/sirupsen/logrus"
	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	apidiscovery "k8s.io/apiserver/pkg/endpoints/discovery"
	"k8s.io/client-go/discovery"
	apiregv1 "k8s.io/kube-aggregator/pkg/apis/apiregistration/v1"
)

var queueRefreshDelay = time.Duration(500)

// APIResourceWatcher provides access to the Kubernetes schema.
type APIResourceWatcher interface {
	List() []metav1.APIResource
	Get(resource, group string) (metav1.APIResource, bool)
	GetKindForResource(gvr schema.GroupVersionResource) string
}

type apiResourceWatcher struct {
	sync.RWMutex

	toSync       int32
	client       discovery.DiscoveryInterface
	apiResources []metav1.APIResource
	gvrToKind    map[schema.GroupVersionResource]string
	resourceMap  map[string]metav1.APIResource
}

// WatchAPIResources creates an APIResourceWatcher object and starts watches on CRDs and APIServices,
// which prompts it to run a discovery check to get the most up to date Kubernetes schema.
func WatchAPIResources(ctx context.Context, discovery discovery.DiscoveryInterface, crd apiextcontrollerv1.CustomResourceDefinitionController, apiService apiregcontrollerv1.APIServiceController) APIResourceWatcher {
	a := &apiResourceWatcher{
		client:      discovery,
		gvrToKind:   make(map[schema.GroupVersionResource]string),
		resourceMap: make(map[string]metav1.APIResource),
	}

	crd.OnChange(ctx, "hns-api", a.OnChangeCRD)
	apiService.OnChange(ctx, "hns-api", a.OnChangeAPIService)
	return a
}

// OnChangeCRD queues the refresh when a CRD event occurs.
func (a *apiResourceWatcher) OnChangeCRD(_ string, crd *apiextv1.CustomResourceDefinition) (*apiextv1.CustomResourceDefinition, error) {
	a.queueRefresh()
	return crd, nil
}

// OnChangeAPIService queues the refresh when an APIService event occurs.
func (a *apiResourceWatcher) OnChangeAPIService(_ string, apiService *apiregv1.APIService) (*apiregv1.APIService, error) {
	a.queueRefresh()
	return apiService, nil
}

// GetKindForResource returns the resource Kind given its GVR.
func (a *apiResourceWatcher) GetKindForResource(gvr schema.GroupVersionResource) string {
	a.RLock()
	defer a.RUnlock()
	return a.gvrToKind[gvr]
}

// List returns all the APIResources for the hns API.
func (a *apiResourceWatcher) List() []metav1.APIResource {
	a.RLock()
	defer a.RUnlock()
	return a.apiResources
}

// Get returns an APIResource and an existence bool given a resource and group.
func (a *apiResourceWatcher) Get(resource, group string) (metav1.APIResource, bool) {
	a.RLock()
	defer a.RUnlock()
	if group == "" {
		val, ok := a.resourceMap[resource]
		return val, ok
	}
	val, ok := a.resourceMap[group+"."+resource]
	return val, ok
}

func (a *apiResourceWatcher) queueRefresh() {
	atomic.StoreInt32(&a.toSync, 1)

	go func() {
		time.Sleep(queueRefreshDelay * time.Millisecond)
		if err := a.refreshAll(); err != nil {
			logrus.Errorf("failed to sync schemas: %v", err)
			atomic.StoreInt32(&a.toSync, 1)
		}
	}()
}

func (a *apiResourceWatcher) setAPIResources() error {
	resourceList, err := discovery.ServerPreferredNamespacedResources(a.client)
	if err != nil {
		return err
	}
	result := []metav1.APIResource{}
	a.Lock()
	defer a.Unlock()
	for _, resource := range resourceList {
		if resource.GroupVersion == consts.GroupVersion {
			continue
		}
		gv := strings.Split(resource.GroupVersion, "/")
		group := ""
		version := ""
		if len(gv) > 1 {
			group = gv[0]
			version = gv[1]
		} else {
			version = gv[0]
		}
		for _, r := range resource.APIResources {
			name := r.Name
			if group != "" {
				name = group + "." + name
			}
			resource := metav1.APIResource{
				Name:               name,
				Group:              group,
				Version:            version,
				Kind:               r.Kind,
				Verbs:              []string{"list", "watch"},
				Namespaced:         true,
				ShortNames:         r.ShortNames,
				StorageVersionHash: apidiscovery.StorageVersionHash(group, version, r.Kind),
			}
			result = append(result, resource)
			a.gvrToKind[schema.GroupVersionResource{Group: group, Version: version, Resource: r.Name}] = r.Kind
			a.resourceMap[name] = resource
		}
	}
	a.apiResources = result
	return nil
}

func (a *apiResourceWatcher) refreshAll() error {
	if !a.needToSync() {
		return nil
	}

	logrus.Infof("Refreshing all types")
	err := a.setAPIResources()
	if err != nil {
		return err
	}

	return nil
}

func (a *apiResourceWatcher) needToSync() bool {
	old := atomic.SwapInt32(&a.toSync, 0)
	return old == 1
}
